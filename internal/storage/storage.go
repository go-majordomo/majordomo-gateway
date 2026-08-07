// Package storage stores request/response bodies in an object store (S3 or GCS) as
// gzipped JSON, keyed by request id. It uses a single bucket configured via env and
// the cloud SDK's default credential chain — no per-tenant configs, no secret store.
package storage

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// Store persists and retrieves request/response body payloads.
type Store interface {
	// Upload writes the payload at key. Implementations gzip the JSON.
	Upload(ctx context.Context, key string, p *BodyPayload) error
	// Download reads and decodes the payload at key.
	Download(ctx context.Context, key string) (*BodyContent, error)
}

// BodyPayload is what the proxy hands the store for one request.
type BodyPayload struct {
	RequestID       uuid.UUID
	Timestamp       time.Time
	RequestMethod   string
	RequestPath     string
	RequestHeaders  map[string]string
	RequestBody     []byte
	ResponseStatus  int
	ResponseHeaders map[string]string
	ResponseBody    []byte
}

// BodyContent is the stored object, decoded.
type BodyContent struct {
	RequestID string          `json:"request_id"`
	Timestamp string          `json:"timestamp"`
	Request   RequestContent  `json:"request"`
	Response  ResponseContent `json:"response"`
}

type RequestContent struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
}

type ResponseContent struct {
	StatusCode int               `json:"status_code"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       json.RawMessage   `json:"body,omitempty"`
}

// GenerateKey builds a deterministic object key for a request's bodies, partitioned
// by date for easy lifecycle rules. An optional prefix namespaces the objects.
func GenerateKey(prefix string, apiKeyID, requestID uuid.UUID, ts time.Time) string {
	key := fmt.Sprintf("%s/%s/%s.json.gz", ts.UTC().Format("2006/01/02"), apiKeyID, requestID)
	if prefix != "" {
		return prefix + "/" + key
	}
	return key
}

// ExtractResponseHeaders flattens an http.Header into a single-value-per-key map.
func ExtractResponseHeaders(h http.Header) map[string]string {
	result := make(map[string]string)
	for key, values := range h {
		if len(values) > 0 {
			result[key] = values[0]
		}
	}
	return result
}

// encode marshals a payload to gzipped JSON.
func encode(p *BodyPayload) ([]byte, error) {
	content := BodyContent{
		RequestID: p.RequestID.String(),
		Timestamp: p.Timestamp.UTC().Format(time.RFC3339),
		Request: RequestContent{
			Method:  p.RequestMethod,
			Path:    p.RequestPath,
			Headers: p.RequestHeaders,
			Body:    toJSONRawMessage(p.RequestBody),
		},
		Response: ResponseContent{
			StatusCode: p.ResponseStatus,
			Headers:    p.ResponseHeaders,
			Body:       toJSONRawMessage(p.ResponseBody),
		},
	}
	jsonData, err := json.Marshal(content)
	if err != nil {
		return nil, fmt.Errorf("marshal body content: %w", err)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(jsonData); err != nil {
		return nil, fmt.Errorf("gzip write: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("gzip close: %w", err)
	}
	return buf.Bytes(), nil
}

// decode gunzips and unmarshals a stored object.
func decode(compressed []byte) (*BodyContent, error) {
	gz, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("gzip reader: %w", err)
	}
	defer gz.Close()

	decompressed, err := io.ReadAll(gz)
	if err != nil {
		return nil, fmt.Errorf("gzip decompress: %w", err)
	}

	var content BodyContent
	if err := json.Unmarshal(decompressed, &content); err != nil {
		return nil, fmt.Errorf("unmarshal body content: %w", err)
	}
	return &content, nil
}

// toJSONRawMessage returns data as-is when it is valid JSON, otherwise as a JSON string.
func toJSONRawMessage(data []byte) json.RawMessage {
	if len(data) == 0 {
		return nil
	}
	if json.Valid(data) {
		return json.RawMessage(data)
	}
	escaped, _ := json.Marshal(string(data))
	return json.RawMessage(escaped)
}
