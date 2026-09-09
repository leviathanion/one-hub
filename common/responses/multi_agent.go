package responses

import (
	"encoding/json"
)

// ProjectMultiAgentEnabled reads only the request semantic needed by the
// Responses WebSocket turn state. Shapes the proxy cannot positively
// interpret remain disabled locally and are still passed raw to the provider.
func ProjectMultiAgentEnabled(raw json.RawMessage) (bool, error) {
	if len(raw) == 0 {
		return false, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return false, nil
	}
	var enabled bool
	if err := json.Unmarshal(object["enabled"], &enabled); err != nil {
		return false, nil
	}
	return enabled, nil
}
