package requestctx

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

type Principal struct {
	Kind     string
	StableID string
}

func PrincipalFromGin(c *gin.Context) Principal {
	if c == nil {
		return Principal{}
	}
	if tokenID := c.GetInt("token_id"); tokenID > 0 {
		return Principal{Kind: "api_key", StableID: strconv.Itoa(tokenID)}
	}
	if userID := c.GetInt("id"); userID > 0 {
		return Principal{Kind: "user", StableID: strconv.Itoa(userID)}
	}
	return Principal{}
}

func (p Principal) IsZero() bool {
	return strings.TrimSpace(p.Kind) == "" || strings.TrimSpace(p.StableID) == ""
}
