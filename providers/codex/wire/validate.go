package wire

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

var (
	originatorPattern      = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	subagentPattern        = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	betaFeatureToken       = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)
	traceparentPattern     = regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$`)
	base64URLOrJWTFragment = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

func validUserAgent(value string) bool { return visibleASCII(value, 1, 256) }
func validID(value string) bool        { return visibleASCII(value, 1, 128) }
func validOriginator(value string) bool {
	return originatorPattern.MatchString(value)
}
func validSubagent(value string) bool {
	return subagentPattern.MatchString(value)
}
func validBoolString(value string) bool {
	return value == "true" || value == "false"
}
func validInstallationID(value string) bool {
	return validID(value)
}
func validPromptCacheKey(value string) bool {
	return visibleASCII(value, 1, 256)
}
func validTurnMetadata(value string) bool {
	return validJSONObjectString(value, 16*1024)
}
func validTurnState(value string) bool {
	return visibleASCII(value, 1, 16*1024)
}
func validBetaFeatures(value string) bool {
	if len(value) > 512 {
		return false
	}
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" || !betaFeatureToken.MatchString(item) {
			return false
		}
	}
	return true
}
func validAttestation(value string) bool {
	if len(value) == 0 || len(value) > 4096 || strings.ContainsAny(value, "\r\n") {
		return false
	}
	if !base64URLOrJWTFragment.MatchString(value) {
		return false
	}
	if strings.Contains(value, ".") {
		return true
	}
	_, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil
}
func validTraceparent(value string) bool {
	if !traceparentPattern.MatchString(value) {
		return false
	}
	parts := strings.Split(value, "-")
	return len(parts) == 4 && parts[1] != "00000000000000000000000000000000" && parts[2] != "0000000000000000"
}
func validTracestate(value string) bool {
	return visibleASCII(value, 1, 512) && !strings.ContainsAny(value, "\r\n")
}
func validUnixMillisString(value string) bool {
	if value == "" || len(value) > 19 {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	return err == nil && parsed >= 0
}

func validJSONObjectString(value string, maxBytes int) bool {
	if len(value) == 0 || len(value) > maxBytes || !visibleASCII(value, 1, maxBytes) {
		return false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(value)), &object); err != nil || object == nil {
		return false
	}
	return true
}

func visibleASCII(value string, minBytes, maxBytes int) bool {
	if len(value) < minBytes || len(value) > maxBytes {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

func singletonValidator(name string) func(string) bool {
	if spec, ok := fieldSpecForHeader(name); ok {
		return spec.valid
	}
	return nil
}

func validateSingletonHeaders(headers HeaderSnapshot) error {
	for _, spec := range fieldSpecs {
		if !spec.singleton {
			continue
		}
		value := headers.Singleton(spec.header, spec.valid)
		switch value.State {
		case FieldMultiple:
			return reject(spec.header, "singleton header has multiple values")
		case FieldInvalid:
			return reject(spec.header, "header value is invalid")
		}
	}
	return nil
}
