"""Offline SDK parse -> accumulate -> serialize -> next-request verification.

From the repository root, using an isolated Python environment:
  pip install anthropic==1.14.0 openai==3.31.0 httpx2==2.13.1
  go build -o work/ag-sdk ./providers/antigravity/wire/testdata/sdk_roundtrip
  python providers/antigravity/wire/testdata/sdk_roundtrip/sdk_roundtrip.py work/ag-sdk

Uses actual SDK clients and MockTransport; no external API is called. Unknown
carrier fields must survive the SDK's own streaming accumulators. Expected
upstream parts are independent fixtures, including integers beyond IEEE-754.
"""

import json
import subprocess
import sys

import anthropic
import httpx2
import openai


class ByteStream(httpx2.SyncByteStream):
    def __init__(self, data):
        self.data = data

    def __iter__(self):
        for i in range(len(self.data)):
            yield self.data[i : i + 1]


class Bridge:
    def __init__(self, path):
        self.process = subprocess.Popen(
            [path], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True
        )

    def call(self, **command):
        self.process.stdin.write(json.dumps(command, ensure_ascii=False) + "\n")
        self.process.stdin.flush()
        line = self.process.stdout.readline()
        assert line, "Go bridge exited unexpectedly"
        result = json.loads(line)
        assert "error" not in result, result
        return result

    def close(self):
        self.process.stdin.close()
        self.process.stdout.close()
        assert self.process.wait(timeout=10) == 0


def fixture(turn):
    parts = [
        {"text": "思考" if turn != 1 else "", "thought": True,
         "thoughtSignature": f"ag1.native-signature-{turn}"},
        {"text": "before" if turn < 2 else "done"},
    ]
    if turn < 2:
        parts += [
            {"functionCall": {"id": f"a{turn}", "name": "lookup",
                              "args": {"n": 9007199254740993}},
             "thoughtSignature": f"tool-a-{turn}"},
            {"thoughtSignature": f"late-a-{turn}"},
            {"functionCall": {"id": f"b{turn}", "name": "lookup",
                              "args": {"query": "第二个"}},
             "thoughtSignature": f"tool-b-{turn}"},
            {"thoughtSignature": f"late-b-{turn}"},
            {"text": "after"},
        ]
    # Separate events place independent signatures after their tools.
    events = [{"response": {"candidates": [{"content": {"parts": [p]}}]}}
              for p in parts]
    events += [
        {"response": {"candidates": [{"finishReason": "STOP"}]}},
        {"response": {"usageMetadata": {"promptTokenCount": 10,
                                         "candidatesTokenCount": 4,
                                         "thoughtsTokenCount": 2,
                                         "cachedContentTokenCount": 3,
                                         "totalTokenCount": 16}}},
    ]
    return parts, events


def verify(bridge, dialect, request, expected, expected_thinking=None):
    target = bridge.call(dialect=dialect, request=request)["body"]
    contents = [{"role": "user", "parts": [{"text": "hello"}]}]
    for turn, parts in enumerate(expected):
        contents.append({"role": "model", "parts": parts})
        if turn < 2:
            contents.append({"role": "user", "parts": [
                {"functionResponse": {"id": f"{p}{turn}", "name": "lookup",
                                      "response": {"result": f"result-{p}{turn}"}}}
                for p in ("b", "a")]})
    assert target["contents"] == contents, (dialect, target["contents"], contents)
    if expected_thinking is not None:
        actual = target.get("generationConfig", {}).get("thinkingConfig")
        assert actual == expected_thinking, (dialect, actual, expected_thinking)


def run(bridge, dialect, streaming):
    expected = []
    history = [{"role": "user", "content": "hello"}]
    calls = 0

    def handle(request):
        nonlocal calls
        raw = json.loads(request.content)
        verify(bridge, dialect, raw, expected,
               {"thinkingBudget": 1024, "includeThoughts": True} if dialect == "claude" else None)
        parts, events = fixture(calls)
        expected.append(parts)
        encoded = bridge.call(dialect=dialect, events=events, stream=streaming)
        calls += 1
        if streaming:
            return httpx2.Response(200, headers={"content-type": "text/event-stream"},
                                   stream=ByteStream("".join(encoded["frames"]).encode()))
        return httpx2.Response(200, json=encoded["body"])

    http = httpx2.Client(transport=httpx2.MockTransport(handle))
    client = (anthropic.Anthropic if dialect == "claude" else openai.OpenAI)(
        api_key="offline-fixture", http_client=http, max_retries=0
    )
    try:
        for turn in range(3):
            if dialect == "claude":
                args = dict(model="fixture-model", max_tokens=2048, messages=history,
                            thinking={"type": "enabled", "budget_tokens": 1024,
                                      "display": "summarized"},
                            tools=[{"name": "lookup", "input_schema": {"type": "object"},
                                    "cache_control": None, "eager_input_streaming": False}])
                if streaming:
                    with client.messages.stream(**args) as stream:
                        message = stream.get_final_message()
                else:
                    message = client.messages.create(**args)
                history.append({"role": "assistant", "content": [
                    b.model_dump(mode="json", exclude_none=True) for b in message.content]})
                if turn < 2:
                    history.append({"role": "user", "content": [
                        {"type": "tool_result", "tool_use_id": f"{p}{turn}",
                         "content": f"result-{p}{turn}"} for p in ("b", "a")]})
                assert message.usage.output_tokens == 6
            elif dialect == "responses":
                args = dict(model="fixture-model", store=False, input=history,
                            prompt_cache_key=None)
                if streaming:
                    with client.responses.stream(**args) as stream:
                        response = stream.get_final_response()
                else:
                    response = client.responses.create(**args)
                history.extend(i.model_dump(mode="json", exclude_none=True)
                               for i in response.output)
                if turn < 2:
                    history.extend({"type": "function_call_output", "call_id": f"{p}{turn}",
                                    "output": f"result-{p}{turn}"} for p in ("b", "a"))
                assert response.usage.output_tokens == 6
            else:
                args = dict(model="fixture-model", messages=history, prompt_cache_key=None)
                if streaming:
                    with client.chat.completions.stream(**args, stream_options={"include_usage": True}) as stream:
                        response = stream.get_final_completion()
                else:
                    response = client.chat.completions.create(**args)
                message = response.choices[0].message.model_dump(mode="json", exclude_none=True)
                # SDK-only parsed metadata is not request history.
                message.pop("parsed", None)
                if message.get("refusal") is None:
                    message.pop("refusal", None)
                history.append(message)
                if turn < 2:
                    history.extend({"role": "tool", "tool_call_id": f"{p}{turn}",
                                    "content": f"result-{p}{turn}"} for p in ("b", "a"))
                assert response.usage.completion_tokens == 6
        if dialect == "responses":
            request = dict(model="fixture-model", store=False, input=history)
        else:
            request = dict(model="fixture-model", messages=history)
        verify(bridge, dialect, request, expected)
        assert calls == 3, calls
        print(f"PASS {dialect} stream={streaming}: 3 turns, parallel calls, late signatures, usage")
    finally:
        client.close()


if __name__ == "__main__":
    print(f"anthropic={anthropic.__version__} openai={openai.__version__} httpx2={httpx2.__version__}")
    bridge = Bridge(sys.argv[1])
    try:
        for dialect in ("claude", "responses", "chat"):
            for streaming in (False, True):
                run(bridge, dialect, streaming)
    finally:
        bridge.close()
