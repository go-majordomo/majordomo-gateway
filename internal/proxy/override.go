package proxy

import (
	"encoding/json"
	"fmt"
)

// OverrideModel replaces the "model" field in a JSON request body.
// Other fields are preserved. Returns an error if the body is not valid JSON.
// Used by the deprecated-model redirect path to rewrite the requested model
// before the request is forwarded upstream.
func OverrideModel(body []byte, newModel string) ([]byte, error) {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("unmarshal request body for model override: %w", err)
	}
	modelJSON, err := json.Marshal(newModel)
	if err != nil {
		return nil, fmt.Errorf("marshal new model: %w", err)
	}
	req["model"] = json.RawMessage(modelJSON)
	return json.Marshal(req)
}
