package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
	"github.com/go-majordomo/majordomo-gateway/internal/pricing"
)

func TestCodexToolsRequireExplicitSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	calc := pricing.NewService("", "../../model_catalog.json", "../../model_aliases.json", time.Hour)
	t.Cleanup(calc.Close)
	project := t.TempDir()
	t.Chdir(project)
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("CODEX_THREAD_ID", "bbbb-2222")
	dir := filepath.Join(home, "sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"aaaa-1111", "bbbb-2222"} {
		meta, err := json.Marshal(map[string]any{
			"type": "session_meta", "payload": map[string]string{"id": id, "cwd": project, "source": "cli"},
		})
		if err != nil {
			t.Fatal(err)
		}
		lines := []string{
			string(meta),
			`{"timestamp":"2026-11-01T12:00:00Z","type":"turn_context","payload":{"model":"gpt-6.2-sol","turn_id":"A"}}`,
			`{"timestamp":"2026-11-01T12:00:00Z","type":"token_usage_record","payload":{"response_id":"r1","usage":{"input_tokens":100,"output_tokens":10},"thread_token_usage":{"total_tokens":110}}}`,
		}
		path := filepath.Join(dir, id+".jsonl")
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		at := time.Unix(1000+int64(i), 0)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	imported := make(chan string, 2)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var imp models.ClaudeCodeImport
		if err := json.NewDecoder(r.Body).Decode(&imp); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		imported <- imp.SessionID
		if err := json.NewEncoder(w).Encode(models.ClaudeCodeImportResult{Requests: len(imp.Requests)}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(gateway.Close)
	tools := &claudeCodeTools{c: newClient(gateway.URL, ""), apiKey: "test-key",
		pricing: calc}
	tools.once.Do(func() {}) // Keep this test on the bundled catalog.
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	tools.registerAgentTools(server)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, closer := range []io.Closer{ss, cs} {
		t.Cleanup(func() {
			if err := closer.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	for _, tool := range []string{"codex_explain", "codex_import"} {
		for _, args := range []map[string]any{{}, {"session_id": ""}, {"session_id": " "}} {
			result, callErr := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
			if callErr == nil && (result == nil || !result.IsError) {
				t.Fatalf("%s accepted an unspecified session: %+v", tool, result)
			}
			body, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			detail := string(body)
			if callErr != nil {
				detail += callErr.Error()
			}
			if !strings.Contains(detail, "session_id") {
				t.Fatalf("%s did not report the missing session_id: %s", tool, detail)
			}
		}
	}
	if len(imported) != 0 {
		t.Fatal("an unspecified session was imported")
	}
	for _, id := range []string{"aaaa-1111", "aaaa"} {
		result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "codex_import", Arguments: map[string]any{"session_id": id}})
		if err != nil || result.IsError {
			t.Fatalf("explicit session import: %+v, %v", result, err)
		}
		if got := <-imported; got != "aaaa-1111" {
			t.Fatalf("imported %q instead of the selected older session", got)
		}
	}
}
