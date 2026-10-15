"""Actual provider/framer/rewrite -> google-genai synchronous SSE parser.

Build the bridge and install the dependencies from sdk_roundtrip.py, then:
  pip install google-genai==2.31.0 httpx==0.28.1
  python providers/antigravity/wire/testdata/sdk_roundtrip/sdk_sse.py work/ag-sdk

No external API is called. Comment handling is a known client limitation,
checked separately so it is not mistaken for a newline regression.
"""

import json
from itertools import product
import sys

import httpx
from google import genai
from google.genai._api_client import HttpResponse
from google.genai.errors import UnknownApiResponseError

from sdk_roundtrip import Bridge


class Chunks(httpx.SyncByteStream):
    def __init__(self, chunks):
        self.chunks = chunks

    def __iter__(self):
        yield from self.chunks


def parse(chunks):
    response = httpx.Response(200, headers={"content-type": "text/event-stream"},
                              stream=Chunks(chunks))
    try:
        return list(HttpResponse(response.headers, response_stream=response).segments())
    finally:
        response.close()


def run(bridge):
    count = 0
    payloads = [
        '{\n"candidates":[{"content":{"parts":[{"text":"你好"}]},"finishReason":"STOP"}]\n}',
        '{"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}',
    ]
    expected = [json.loads(p) for p in payloads]
    for ending in ("\n", "\r\n", "\r"):
        def data(payload):
            return "data: " + payload.replace("\n", ending + "data: ") + ending + ending
        source = "".join(data('{"response":' + p + '}') for p in payloads)
        output = "".join(bridge.call(native_sse=source)["frames"])
        assert output == "".join(data(p) for p in payloads)
        wire = output.encode()
        chunkings = [[wire]] + [[wire[:i], wire[i:]] for i in range(1, len(wire))]
        chunkings.append([wire[i:i + 1] for i in range(len(wire))])
        for chunks in chunkings:
            assert parse(chunks) == expected, (repr(ending), len(chunks))
            count += 1
        print(f"PASS provider -> SDK ending={ending!r}: {len(chunkings)} chunkings")

    mixed_count = 0
    for first, last, blank in product(("\n", "\r\n", "\r"), repeat=3):
        if last == "\r" and blank == "\n":
            continue  # This pair is one CRLF, not an event separator.
        source = ('data: {"response":' + first + 'data: '
                  + json.dumps(expected[0], ensure_ascii=False) + '}' + last + blank)
        source += 'data: {"response":' + payloads[1] + '}\n\n'
        output = "".join(bridge.call(native_sse=source)["frames"]).encode()
        chunkings = [[output]] + [[output[:i], output[i:]] for i in range(1, len(output))]
        chunkings.append([output[i:i + 1] for i in range(len(output))])
        for chunks in chunkings:
            assert parse(chunks) == expected, (repr(first), repr(last), repr(blank), len(chunks))
            mixed_count += 1
    print(f"PASS mixed data/blank endings: {mixed_count} chunkings, two distinct events")

    # The provider must preserve legal comments even though this pinned SDK
    # rejects them for both LF and CRLF. Do not clear metadata for success.
    for ending in ("\n", "\r\n"):
        comment = ": keepalive" + ending + ending
        source = comment + "data: {\"response\":" + json.dumps(expected[0]) + "}" + ending + ending
        output = "".join(bridge.call(native_sse=source)["frames"])
        assert output.startswith(comment)
        try:
            parse([output.encode()])
        except UnknownApiResponseError:
            print(f"KNOWN SDK LIMITATION comment ending={ending!r}: UnknownApiResponseError")
        else:
            raise AssertionError("SDK comment behavior changed; update the documented boundary")
    print(f"PASS {count + mixed_count} payload/usage/EOF chunkings; comment limitation reproduced")


if __name__ == "__main__":
    print(f"google-genai={genai.__version__} httpx={httpx.__version__}")
    bridge = Bridge(sys.argv[1])
    try:
        run(bridge)
    finally:
        bridge.close()
