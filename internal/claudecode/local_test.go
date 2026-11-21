package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChildEnvKeepsConfig(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	t.Setenv(CurrentSessionEnv, "abc")
	t.Setenv("CLAUDE_CONFIG_DIR", "/custom")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")

	env := strings.Join(ChildEnv(), "\n")
	for _, dropped := range []string{"CLAUDECODE=", CurrentSessionEnv + "="} {
		if strings.Contains(env, dropped) {
			t.Errorf("%s should be removed (nested-session marker)", dropped)
		}
	}
	for _, kept := range []string{"CLAUDE_CONFIG_DIR=/custom", "CLAUDE_CODE_USE_BEDROCK=1"} {
		if !strings.Contains(env, kept) {
			t.Errorf("%s should be kept (configuration)", kept)
		}
	}
}

func TestMCPMeasurementDefinitions(t *testing.T) {
	m := &MCPMeasurement{Names: []string{"a", "b"}, Alone: map[string]int{"a": 1000, "b": 500}, Combined: 1100, Shared: 400}
	if m.Alongside("a") != 600 || m.Alongside("b") != 100 {
		t.Errorf("alongside = %d, %d", m.Alongside("a"), m.Alongside("b"))
	}
	if d := m.Definitions(); d.Total() != 1100 {
		t.Errorf("definitions total = %d, want the combined measurement 1100", d.Total())
	}
}

func TestResolveSession(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"projA/aaaa1111.jsonl", "projB/aaaa2222.jsonl", "projB/bbbb3333.jsonl"} {
		writeFile(t, filepath.Join(root, p), prompt("hi"))
	}
	if p, err := ResolveSession(root, "/nowhere", "bbbb"); err != nil || filepath.Base(p) != "bbbb3333.jsonl" {
		t.Errorf("prefix lookup across projects = %q, %v", p, err)
	}
	if _, err := ResolveSession(root, "/nowhere", "aaaa"); err == nil {
		t.Error("an ambiguous prefix should be an error")
	}
	if _, err := ResolveSession(root, "/nowhere", "zzzz"); err == nil {
		t.Error("an unknown id should be an error")
	}
}

// A server name never becomes part of a path: a hostile name cannot write
// outside the temporary directory, and the name survives inside the JSON.
func TestWriteServerConfigStaysInDir(t *testing.T) {
	dir := t.TempDir()
	path, err := writeServerConfig(dir, "../../escape", []byte(`{"command":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != dir {
		t.Fatalf("config written to %s, outside %s", path, dir)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"../../escape"`) {
		t.Errorf("server name missing from config: %s", data)
	}
}

func TestSessionIDFormat(t *testing.T) {
	for _, id := range []string{"invalid", "a/b", "a*"} {
		if _, err := ResolveSession(t.TempDir(), ".", id); err == nil || err.Error() != "invalid session ID or prefix" {
			t.Errorf("invalid ID accepted: %q (%v)", id, err)
		}
	}
}
func TestPromptPreviewUnicode(t *testing.T) {
	if got := snippet("京都駅前", 3); got != "京都…" {
		t.Fatalf("preview = %q", got)
	}
}

func TestLoadSessionRejectsExternalSymlink(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "empty.jsonl")
	writeFile(t, outside)
	path := filepath.Join(root, "abc.jsonl")
	if err := os.Symlink(outside, path); err != nil {
		t.Skip(err)
	}
	if _, err := LoadSession(root, path); err == nil {
		t.Fatal("session reader followed an external link")
	}
}
