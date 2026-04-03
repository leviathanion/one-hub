package authutil

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
)

func StableRequestCredentialNamespace(req *http.Request) string {
	credential := ExtractStableRequestCredential(req)
	if credential.Empty() {
		return ""
	}

	sum := sha256.Sum256([]byte(credential.Value))
	return "auth:" + hex.EncodeToString(sum[:8])
}
