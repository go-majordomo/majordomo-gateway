package sessionclient

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
)

// Import sends usage to the gateway using the importing key.
func Import(client *http.Client, baseURL, source, key string, imp *models.ClaudeCodeImport) (*models.ClaudeCodeImportResult, error) {
	data, err := json.Marshal(imp)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/"+source+"/import", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Majordomo-Key", key)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // Read-only resource; read errors are checked below.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errors.New("unauthorized — check the gateway API key (--key or MAJORDOMO_API_KEY)")
	}
	if resp.StatusCode >= 400 {
		var apiErr struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &apiErr) == nil && apiErr.Error != "" {
			return nil, errors.New(apiErr.Error)
		}
		return nil, fmt.Errorf("server returned %d", resp.StatusCode)
	}
	var res models.ClaudeCodeImportResult
	return &res, json.Unmarshal(body, &res)
}
