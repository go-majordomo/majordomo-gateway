package agentusage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/go-majordomo/majordomo-gateway/internal/claudecode"
)

type ProbeResult struct {
	Projection *Overhead      `json:"projection,omitempty"`
	Source     string         `json:"source"`
	Model      string         `json:"model"`
	Baseline   int            `json:"baseline_context_tokens"`
	Alone      map[string]int `json:"additional_tokens_alone"`
	Combined   int            `json:"additional_tokens_combined"`
	Note       string         `json:"note"`
}

// Probe measures the first model context in otherwise identical, isolated
// client invocations. Keep raw deltas: deferred catalogs and run-to-run noise
// can produce a small negative number.
func Probe(ctx context.Context, source, model, path string) (ProbeResult, error) {
	result := ProbeResult{Source: source, Model: model, Alone: map[string]int{}, Note: "Observed context deltas for identical short prompts. Deferred tools and run-to-run variation can produce small or negative differences."}
	servers, err := readServers[map[string]any](path)
	if err != nil {
		return result, err
	}
	var names []string
	for name, c := range servers {
		if enabled, ok := c["enabled"].(bool); ok && !enabled {
			delete(servers, name)
			continue
		}
		names = append(names, name)
		if source == "codex" {
			c["required"] = true
		}
	}
	if len(names) == 0 {
		return result, errors.New("no enabled MCP servers")
	}
	dir, err := os.MkdirTemp("", "majordomo-probe-")
	if err != nil {
		return result, err
	}
	defer removeProbeDir(dir)
	measurement, err := claudecode.MeasureDefinitions(names, func(selected []string) (int, string, error) {
		config := make(map[string]map[string]any, len(selected))
		for _, name := range selected {
			config[name] = servers[name]
		}
		count, err := probeContext(ctx, source, model, dir, config)
		return count, model, err
	})
	if err != nil {
		return result, err
	}
	result.Baseline, result.Alone, result.Combined = measurement.Baseline, measurement.Alone, measurement.Combined
	return result, nil
}

func probeContext(ctx context.Context, source, model, work string, servers map[string]map[string]any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	// Start a fresh history for every run. Only the selected server configuration
	// reaches the client. Do not put credentials from that config on its argv.
	home, err := os.MkdirTemp("", "majordomo-client-")
	if err != nil {
		return 0, err
	}
	defer removeProbeDir(home)
	const prompt = "Respond with OK. Do not use tools."
	var cmd *exec.Cmd
	auth := filepath.Join(filepath.Dir(DefaultPath("codex")), "auth.json")
	if _, err := os.Stat(auth); err == nil {
		if err = os.Symlink(auth, filepath.Join(home, "auth.json")); err != nil {
			return 0, err
		}
	}
	data, err := toml.Marshal(map[string]any{"mcp_servers": servers})
	if err != nil {
		return 0, err
	}
	if err = os.WriteFile(filepath.Join(home, "config.toml"), data, 0600); err != nil {
		return 0, err
	}
	cmd = exec.CommandContext(ctx, "codex", "exec", "--ephemeral", "--skip-git-repo-check", "--json", "-s", "read-only", "-m", model, prompt)
	cmd.Env = append(os.Environ(), "CODEX_HOME="+home)
	cmd.Dir = work
	data, err = cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, fmt.Errorf("%s probe failed (check CLI login, model and configured servers): %w", source, err)
	}
	var input int
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		var e struct {
			Type  string
			Usage usage
			Item  struct{ Type string }
		}
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		if e.Type == "item.completed" && e.Item.Type != "agent_message" && e.Item.Type != "reasoning" {
			return 0, errors.New("probe used tools; cannot isolate schema overhead")
		}
		if e.Type == "turn.completed" || e.Type == "result" {
			input = e.Usage.Input
		}
	}
	if input > 0 {
		return input, nil
	}
	return 0, errors.New("client did not return a measured context count")
}

// ApplyMeasurement uses the measured combined delta as the budget and
// distributes it across servers in proportion to their individual deltas.
// Measurements only apply to requests using the probed model.
func (s *Session) ApplyMeasurement(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var p ProbeResult
	if err = json.Unmarshal(data, &p); err != nil {
		return err
	}
	if p.Source != s.Source || p.Model == "" || p.Baseline <= 0 || len(p.Alone) == 0 {
		return errors.New("expected a live probe for this client")
	}
	defs := map[string]int{}
	names := make([]string, 0, len(p.Alone))
	weight := 0
	for name, delta := range p.Alone {
		names = append(names, name)
		weight += max(delta, 0)
	}
	sort.Strings(names)
	used, assigned := 0, 0
	for _, name := range names {
		used += max(p.Alone[name], 0)
		next := int(float64(used) * float64(max(0, p.Combined)) / float64(max(weight, 1)))
		defs[name] = next - assigned
		assigned = next
	}
	if weight == 0 && p.Combined > 0 {
		defs["(combined MCP overhead)"] = p.Combined
	}
	matched := false
	for i := range s.Requests {
		if s.Requests[i].Model == p.Model {
			s.Requests[i].Definitions = defs
			matched = true
		}
	}
	if !matched {
		return fmt.Errorf("session has no requests using the measured model %q", p.Model)
	}
	return nil
}

func removeProbeDir(path string) {
	if err := os.RemoveAll(path); err != nil {
		slog.Warn("remove probe directory", "path", path, "error", err)
	}
}
