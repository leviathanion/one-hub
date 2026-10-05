package wire

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"one-api/common/jsonobject"
	commonresponses "one-api/common/responses"
)

var requestProcessingBenchmarkSink any

// BenchmarkRequestProcessing measures local cumulative allocation, not retained
// heap or upstream latency. CreateLocalChain includes the original exact-key
// resource extraction, but excludes authorization SQL, tokenization and I/O.
// Run with: go test ./providers/codex/wire -run '^$' -bench BenchmarkRequestProcessing -benchmem
func BenchmarkRequestProcessing(b *testing.B) {
	text := strings.Repeat("x", 1<<20)
	for _, fixture := range []struct {
		name string
		raw  string
	}{
		{"Short1KiB", `{"model":"gpt-5","input":"` + strings.Repeat("x", 1<<10) + `","stream":true,"store":false}`},
		{"Text1MiB", `{"model":"gpt-5","input":"` + text + `","stream":true,"store":false}`},
		{"Image1MiB", `{"model":"gpt-5","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,` + text + `"},{"type":"input_file","file_id":"file-owned"}]}]}`},
		{"Schema1MiB", `{"model":"gpt-5","input":"hello","tools":[{"type":"function","name":"tool","parameters":{"description":"` + text + `","n":12345678901234567890}},{"type":"file_search","vector_store_ids":["vs-owned"]}]}`},
	} {
		b.Run(fixture.name, func(b *testing.B) {
			raw := []byte(fixture.raw)
			object, err := jsonobject.Parse(raw)
			if err != nil {
				b.Fatal(err)
			}
			for _, stage := range []string{"ParseObject", "CreateBody", "CompactBody", "CreateLocalChain"} {
				b.Run(stage, func(b *testing.B) {
					b.ReportAllocs()
					b.SetBytes(int64(len(raw)))
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						switch stage {
						case "ParseObject":
							value, err := jsonobject.Parse(raw)
							if err != nil {
								b.Fatal(err)
							}
							requestProcessingBenchmarkSink = value
						case "CreateBody":
							value, err := PlanResponsesCreateBody(object, CreateBodyInput{Model: "gpt-5", Stream: true})
							if err != nil {
								b.Fatal(err)
							}
							requestProcessingBenchmarkSink = value
						case "CompactBody":
							value, err := PlanResponsesCompactBody(object, "gpt-5", nil)
							if err != nil {
								b.Fatal(err)
							}
							requestProcessingBenchmarkSink = value
						case "CreateLocalChain":
							envelope, err := commonresponses.ParseRawEnvelope(raw)
							if err != nil {
								b.Fatal(err)
							}
							// Keep the same full-map decoding and exact-key visitor as
							// relay.prepareResourceRequest; typed projection is not
							// equivalent for case-distinct extension fields.
							var resourceBody map[string]any
							decoder := json.NewDecoder(bytes.NewReader(raw))
							decoder.UseNumber()
							if err := decoder.Decode(&resourceBody); err != nil {
								b.Fatal(err)
							}
							refs := commonresponses.ProtocolResourceReferences(resourceBody, "responses")
							value, err := PlanResponsesCreateBody(envelope.Object, CreateBodyInput{Model: "gpt-5", Stream: true})
							if err != nil {
								b.Fatal(err)
							}
							requestProcessingBenchmarkSink = struct {
								Body []byte
								Refs []commonresponses.ResourceReference
							}{value, refs}
						}
					}
				})
			}
		})
	}
}
