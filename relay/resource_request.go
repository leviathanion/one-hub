package relay

import (
	"bytes"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"one-api/common/requestctx"
	commonresponses "one-api/common/responses"
	"one-api/relay/relay_util"
)

// prepareResourceRequest projects only known resource locations. It preserves
// the raw payload and leaves parameter/business validation to the upstream.
func prepareResourceRequest(c *gin.Context, raw []byte, operation string) error {
	policy := relay_util.ResourceReferencePolicy{Context: c}
	requestctx.SetResourceReferencePolicy(c, policy)
	var body map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&body) != nil {
		return nil
	}
	refs := commonresponses.ProtocolResourceReferences(body, operation)
	for _, ref := range refs {
		if err := policy.AuthorizeResourceReference(ref.Kind, ref.ID); err != nil {
			return err
		}
	}
	return nil
}
