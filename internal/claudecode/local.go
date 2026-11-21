package claudecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	gateway "github.com/go-majordomo/majordomo-gateway"
	"github.com/go-majordomo/majordomo-gateway/internal/pricing"
)

// DefaultProjectsDir is where Claude Code keeps session transcripts.
func DefaultProjectsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects")
}

// CurrentSessionEnv is set by Claude Code in the environment of the processes it
// launches (including MCP servers) to the id of the running session.
const CurrentSessionEnv = "CLAUDE_CODE_SESSION_ID"

var sessionPrefix = regexp.MustCompile(`^[0-9a-fA-F-]{1,36}$`)

// ResolveSession returns a session's main transcript path. With an id (or a
// unique id prefix) it searches every project; without one it returns the most
// recent session started in projectDir.
func ResolveSession(root, projectDir, id string) (string, error) {
	if id == "" {
		paths, err := FindSessions(root, projectDir, time.Time{})
		if err != nil {
			return "", err
		}
		if len(paths) == 0 {
			return "", errors.New("no Claude Code session found in this project")
		}
		return paths[0], nil
	}
	if !sessionPrefix.MatchString(id) {
		return "", errors.New("invalid session ID or prefix")
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer func() { _ = dir.Close() }() // Read-only resource; read errors are checked below.
	matches, err := fs.Glob(dir.FS(), "*/"+id+"*.jsonl")
	if err != nil {
		return "", err
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no Claude Code session with id %q", id)
	case 1:
		return filepath.Join(root, filepath.FromSlash(matches[0])), nil
	default:
		return "", fmt.Errorf("session id %q is ambiguous (%d matches)", id, len(matches))
	}
}

// LocalPricing builds the gateway's pricing service from the bundled catalog
// (or MODEL_CATALOG_FILE / PRICING_ALIASES_FILE when set) plus the remote feed,
// so local tools price exactly as the server does. Call the returned func when done.
func LocalPricing() (*pricing.Service, func(), error) {
	// The pricing service logs its loading at info level; only warnings matter here.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))

	tmp, err := os.MkdirTemp("", "majordomo-pricing")
	if err != nil {
		return nil, nil, err
	}
	pathFor := func(env, name string, data []byte) (string, error) {
		if p := os.Getenv(env); p != "" {
			return p, nil
		}
		p := filepath.Join(tmp, name)
		return p, os.WriteFile(p, data, 0o600)
	}
	catalog, err := pathFor("MODEL_CATALOG_FILE", "model_catalog.json", gateway.ModelCatalog)
	if err != nil {
		removeTempDir(tmp)
		return nil, nil, err
	}
	aliases, err := pathFor("PRICING_ALIASES_FILE", "model_aliases.json", gateway.ModelAliases)
	if err != nil {
		removeTempDir(tmp)
		return nil, nil, err
	}
	remote := os.Getenv("PRICING_REMOTE_URL")
	if remote == "" {
		remote = "https://www.llm-prices.com/current-v1.json"
	}
	svc := pricing.NewService(remote, catalog, aliases, time.Hour)
	return svc, func() { svc.Close(); removeTempDir(tmp) }, nil
}

// MCPMeasurement is the tool-definition overhead of the servers in an MCP config.
type MCPMeasurement struct {
	ModelID  string         `json:"model"`
	Baseline int            `json:"baseline_context_tokens"` // no MCP servers
	Names    []string       `json:"servers"`                 // sorted
	Alone    map[string]int `json:"tokens_alone"`            // each server connected alone
	Combined int            `json:"tokens_all_together"`
	Shared   int            `json:"shared_preamble_tokens"` // sent once, included in each "alone"
}

// Alongside is the tokens a server adds when other servers are also connected.
func (m *MCPMeasurement) Alongside(name string) int {
	if len(m.Names) == 1 {
		return m.Alone[name]
	}
	return m.Alone[name] - m.Shared
}

// Definitions converts the measurement for use in a report.
func (m *MCPMeasurement) Definitions() *MCPDefinitions {
	d := &MCPDefinitions{PerServer: map[string]int{}}
	for _, n := range m.Names {
		d.PerServer[n] = m.Alongside(n)
	}
	if len(m.Names) > 1 {
		d.Shared = m.Shared
	}
	return d
}

// MeasureMCP runs a tiny `claude -p` call (on the user's Claude login) with no MCP
// servers, once per server, and once with all of them, and reports how much
// input context each adds.
func MeasureMCP(model, config string) (*MCPMeasurement, error) {
	raw, err := os.ReadFile(config)
	if err != nil {
		return nil, err
	}
	var cfg struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil || len(cfg.MCPServers) == 0 {
		return nil, fmt.Errorf("%s: no mcpServers found", config)
	}

	m := &MCPMeasurement{Alone: map[string]int{}}
	if m.Baseline, m.ModelID, err = measureContext(model, ""); err != nil {
		return nil, fmt.Errorf("baseline run: %w", err)
	}
	tmp, err := os.MkdirTemp("", "mcp-overhead")
	if err != nil {
		return nil, err
	}
	defer removeTempDir(tmp)

	for n := range cfg.MCPServers {
		m.Names = append(m.Names, n)
	}
	sort.Strings(m.Names)
	sum := 0
	for _, name := range m.Names {
		path, err := writeServerConfig(tmp, name, cfg.MCPServers[name])
		if err != nil {
			return nil, err
		}
		ctx, _, err := measureContext(model, path)
		if err != nil {
			return nil, fmt.Errorf("server %s: %w", name, err)
		}
		m.Alone[name] = ctx - m.Baseline
		sum += m.Alone[name]
	}

	// Measured alone, each server also carries a shared MCP preamble that is
	// sent only once when several servers are connected.
	m.Combined = sum
	if len(m.Names) > 1 {
		ctx, _, err := measureContext(model, config)
		if err != nil {
			return nil, fmt.Errorf("all servers: %w", err)
		}
		m.Combined = ctx - m.Baseline
		m.Shared = max((sum-m.Combined)/(len(m.Names)-1), 0)
	}
	return m, nil
}

// measureContext runs a minimal `claude -p` call and returns its total input
// context (input + cache read + cache write) and the resolved model id.
func measureContext(model, mcpConfig string) (int, string, error) {
	args := []string{"-p", "Reply with: OK", "--model", model,
		"--system-prompt", "Reply tersely.", "--tools", "", "--strict-mcp-config",
		"--setting-sources", "", "--disable-slash-commands", "--no-session-persistence",
		"--output-format", "json"}
	if mcpConfig != "" {
		args = append(args, "--mcp-config", mcpConfig)
	}
	cmd := exec.Command("claude", args...)
	cmd.Env = ChildEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return 0, "", fmt.Errorf("claude: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	var res struct {
		IsError    bool   `json:"is_error"`
		Result     string `json:"result"`
		ModelUsage map[string]struct {
			InputTokens         int `json:"inputTokens"`
			CacheReadTokens     int `json:"cacheReadInputTokens"`
			CacheCreationTokens int `json:"cacheCreationInputTokens"`
		} `json:"modelUsage"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return 0, "", err
	}
	if res.IsError || len(res.ModelUsage) != 1 {
		return 0, "", fmt.Errorf("unexpected result: %s", res.Result)
	}
	for id, u := range res.ModelUsage {
		return u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens, id, nil
	}
	return 0, "", errors.New("no usage reported")
}

// ChildEnv is the environment for a child `claude`: the current one minus the
// variables that mark a process as running inside a Claude Code session (which
// would make the child behave as a nested session). Configuration such as
// CLAUDE_CONFIG_DIR or CLAUDE_CODE_USE_BEDROCK is kept.
func ChildEnv() []string {
	nested := map[string]bool{
		"CLAUDECODE": true, "CLAUDE_CODE_ENTRYPOINT": true, CurrentSessionEnv: true,
		"CLAUDE_CODE_CHILD_SESSION": true, "CLAUDE_CODE_SESSION_ATTENDED": true,
		"CLAUDE_CODE_MESSAGING_SOCKET": true, "CLAUDE_CODE_MESSAGING_TOKEN": true,
		"CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS": true, "CLAUDE_CODE_EXECPATH": true,
		"CLAUDE_CODE_EXECUTABLE": true, "CLAUDE_AGENT_SDK_VERSION": true, "CLAUDE_PID": true,
		"CLAUDE_EFFORT": true,
	}
	var env []string
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); !nested[name] {
			env = append(env, kv)
		}
	}
	return env
}

// writeServerConfig writes an MCP config holding one server into dir and
// returns its path. The file name is random: the server name comes from the
// user's config and stays inside the JSON, never in a path.
func writeServerConfig(dir, name string, server json.RawMessage) (string, error) {
	data, err := json.Marshal(map[string]any{"mcpServers": map[string]json.RawMessage{name: server}})
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "server-*.json")
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close() // the write error is the one to report
		return "", err
	}
	return f.Name(), f.Close()
}

func removeTempDir(path string) {
	if err := os.RemoveAll(path); err != nil {
		slog.Warn("remove temporary directory", "path", path, "error", err)
	}
}
