// Offline bridge for sdk_roundtrip.py. No credentials or upstream HTTP are used.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"one-api/providers/antigravity/wire"
)

type command struct {
	Dialect   wire.Dialect      `json:"dialect"`
	Request   json.RawMessage   `json:"request"`
	Events    []json.RawMessage `json:"events"`
	Stream    bool              `json:"stream"`
	NativeSSE *string           `json:"native_sse"`
}

type result struct {
	Body   json.RawMessage `json:"body,omitempty"`
	Frames []string        `json:"frames,omitempty"`
	Error  string          `json:"error,omitempty"`
}

func execute(c command) result {
	if c.NativeSSE != nil {
		return nativeSSE(*c.NativeSSE)
	}
	if len(c.Request) > 0 {
		r, err := wire.ConvertRequest(c.Dialect, c.Request, "fixture-model")
		if err != nil {
			return result{Error: err.Error()}
		}
		if err = wire.FinalizeRequest(r); err != nil {
			return result{Error: err.Error()}
		}
		return result{Body: r.Body}
	}
	d := wire.NewResponseDecoder(&wire.Context{Dialect: c.Dialect, Model: "fixture-model", ExpectedCandidates: 1, IncludeUsage: true}, "fixture-response", c.Stream)
	out := result{}
	for _, e := range c.Events {
		frames, err := d.Push(e)
		if err != nil {
			return result{Error: err.Error()}
		}
		for _, f := range frames {
			out.Frames = append(out.Frames, string(f))
		}
	}
	frames, body, err := d.Finish()
	if err != nil {
		return result{Error: err.Error()}
	}
	out.Body = body
	for _, f := range frames {
		out.Frames = append(out.Frames, string(f))
	}
	return out
}

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), wire.MaxRetainedBytes)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var c command
		if err := json.Unmarshal(scanner.Bytes(), &c); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := encoder.Encode(execute(c)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
