package requestctx

import (
	"fmt"
	"github.com/gin-gonic/gin"
)

// ResourceReferencePolicy keeps authorization in relay while adapters extract
// resource identities from their effective protocol payloads.
type ResourceReferencePolicy interface{ AuthorizeResourceReference(kind, id string) error }

const resourceReferencePolicyKey = "relay.resource_reference_policy"

func SetResourceReferencePolicy(c *gin.Context, policy ResourceReferencePolicy) {
	if c != nil {
		c.Set(resourceReferencePolicyKey, policy)
	}
}
func AuthorizeResourceReference(c *gin.Context, kind, id string) error {
	if c != nil {
		if value, ok := c.Get(resourceReferencePolicyKey); ok {
			if policy, ok := value.(ResourceReferencePolicy); ok {
				return policy.AuthorizeResourceReference(kind, id)
			}
		}
	}
	return fmt.Errorf("%s requires an account-scoped resource owner", kind)
}
