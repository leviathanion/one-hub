package antigravity

import (
	"bytes"
	"encoding/json"
)

func (AntigravityProviderFactory) WholeCredentialInput() bool { return true }

func (AntigravityProviderFactory) PrepareCredentialInput(key string) (string, error) {
	if err := ValidateCredentialKey(key); err != nil {
		return "", err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(key)); err != nil {
		return "", err
	}
	return compact.String(), nil
}
