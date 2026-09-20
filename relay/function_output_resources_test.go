package relay

import (
	"bytes"
	"fmt"
	"testing"

	"one-api/model"
	"one-api/relay/relay_util"
)

func TestResourceRequestFunctionOutputAuthorization(t *testing.T) {
	setupRelayTestDB(t, &model.ResourceOwner{})
	seedRequestResource(t, "file", "file_owned", 1, 7)
	seedRequestResource(t, "file", "file_other", 2, 7)
	for _, kind := range []string{"input_file", "input_image"} {
		for _, id := range []string{"file_owned", "file_other", "file_unknown"} {
			t.Run(kind+"/"+id, func(t *testing.T) {
				raw := []byte(fmt.Sprintf(`{ "model":"gpt-5", "input":[{"type":"function_call_output","call_id":"call_1","output":[{"type":%q,"file_id":%q,"future":1e0}]}] }`, kind, id))
				original := bytes.Clone(raw)
				c := resourceRequestContext(raw, 1)
				err := prepareResourceRequest(c, raw, "responses")
				if id == "file_owned" {
					if err != nil || c.GetInt(relay_util.ResourceOwnerChannelKey) != 7 {
						t.Fatalf("owned tool output route=%d error=%v", c.GetInt(relay_util.ResourceOwnerChannelKey), err)
					}
				} else if err == nil || resourceRequestErrorCode(t, err) != "resource_not_found" {
					t.Fatalf("unauthorized tool output accepted: %v", err)
				}
				if !bytes.Equal(raw, original) {
					t.Fatal("authorization rewrote the request")
				}
			})
		}
	}
}
