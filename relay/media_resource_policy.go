package relay

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"one-api/common/requestctx"
	commonresponses "one-api/common/responses"
	"one-api/relay/relay_util"
)

func authorizeMediaRequest(c *gin.Context, raw []byte, operation string) error {
	policy := relay_util.ResourceReferencePolicy{Context: c}
	requestctx.SetResourceReferencePolicy(c, policy)
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		return nil
	}
	for _, ref := range commonresponses.MediaResourceReferences(body, operation) {
		if err := policy.AuthorizeResourceReference(ref.Kind, ref.ID); err != nil {
			return err
		}
	}
	return nil
}
