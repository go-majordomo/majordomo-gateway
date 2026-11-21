package main

import (
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestJSONResult(t *testing.T) {
	for _, value := range []any{map[string]int{"requests": 1}, json.RawMessage(`{"requests":1}`)} {
		result, structured, err := jsonResult(value)
		if err != nil || structured != nil || result == nil || len(result.Content) != 1 {
			t.Fatalf("JSON result: %+v, %v, %v", result, structured, err)
		}
		content, ok := result.Content[0].(*mcp.TextContent)
		if !ok || content.Text != `{"requests":1}` {
			t.Fatalf("expected JSON object as MCP text, got %+v", result.Content)
		}
	}
	if result, _, err := jsonResult(make(chan int)); err == nil || result != nil {
		t.Fatalf("unsupported value: %+v, %v", result, err)
	}
}
