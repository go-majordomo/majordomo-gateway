package agentusage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pelletier/go-toml/v2"

	"github.com/go-majordomo/majordomo-gateway/internal/claudecode"
	"github.com/go-majordomo/majordomo-gateway/internal/models"
)

type serverConfig struct {
	Command, URL, Cwd, Type string
	Args                    []string
	Env, Headers            map[string]string
	HTTPHeaders             map[string]string `toml:"http_headers"`
	TokenEnv                string            `toml:"bearer_token_env_var"`
	Enabled                 *bool
	EnabledTools            []string `toml:"enabled_tools"`
	DisabledTools           []string `toml:"disabled_tools"`
}
type headerTransport map[string]string

func (t headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range t {
		r.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// CatalogDefinitions lists tools from an explicitly supplied local client config.
// Serialized schemas are an estimate of full catalog exposure with lazy loading off.
func CatalogDefinitions(ctx context.Context, path string) (map[string]int, error) {
	servers, err := readServers[serverConfig](path)
	if err != nil {
		return nil, err
	}
	defs := map[string]int{}
	for name, c := range servers {
		if c.Enabled != nil && !*c.Enabled {
			continue
		}
		n, err := catalogSize(ctx, c)
		if err != nil {
			return nil, fmt.Errorf("list tools for %s: %w", name, err)
		}
		defs[name] = n
	}
	return defs, nil
}

func catalogSize(ctx context.Context, c serverConfig) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var transport mcp.Transport
	switch {
	case c.Command != "":
		cmd := exec.CommandContext(ctx, c.Command, c.Args...)
		cmd.Dir = c.Cwd
		cmd.Env = os.Environ()
		for k, v := range c.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		transport = &mcp.CommandTransport{Command: cmd}
	case c.URL != "":
		h := headerTransport{}
		for k, v := range c.Headers {
			h[k] = v
		}
		for k, v := range c.HTTPHeaders {
			h[k] = v
		}
		if c.TokenEnv != "" {
			h["Authorization"] = "Bearer " + os.Getenv(c.TokenEnv)
		}
		client := &http.Client{Transport: h, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		if c.Type == "sse" {
			transport = &mcp.SSEClientTransport{Endpoint: c.URL, HTTPClient: client}
		} else {
			transport = &mcp.StreamableClientTransport{Endpoint: c.URL, HTTPClient: client, MaxRetries: -1, DisableStandaloneSSE: true}
		}
	default:
		return 0, errors.New("expected a command or URL")
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "gateway-cli", Version: "1"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err := session.Close(); err != nil {
			slog.Warn("close MCP session", "error", err)
		}
	}()
	size := 0
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			return 0, err
		}
		if c.EnabledTools != nil && !slices.Contains(c.EnabledTools, tool.Name) || slices.Contains(c.DisabledTools, tool.Name) {
			continue
		}
		payload, err := json.Marshal(struct {
			Name        string `json:"name"`
			Description string `json:"description,omitempty"`
			Parameters  any    `json:"parameters"`
		}{tool.Name, tool.Description, tool.InputSchema})
		if err != nil {
			return 0, err
		}
		size += tokens(len(payload))
	}
	return size, nil
}

func (s *Session) ApplyCatalog(defs map[string]int) {
	for i := range s.Requests {
		s.Requests[i].Definitions = defs
	}
}

type Projection struct {
	Server   string  `json:"server"`
	Tokens   int     `json:"tokens_per_request"`
	Cached   float64 `json:"cached_cost"`
	Uncached float64 `json:"uncached_cost"`
	Priced   bool    `json:"priced"`
}
type Overhead struct {
	Model    string `json:"model"`
	Requests int    `json:"requests"`
	// PromptTokens is the prompt size the schema tokens ride in, which selects
	// the price tier; zero when no prompt was measured.
	PromptTokens int          `json:"prompt_tokens,omitempty"`
	Servers      []Projection `json:"servers"`
	Note         string       `json:"note"`
}

// Project prices each server's schema tokens over n requests. Only the schema
// tokens are charged, at the price tier of a promptTokens-token prompt (the
// measured request size); zero assumes a prompt below any long-context
// threshold, and the note says so.
func Project(calc claudecode.Calculator, source, model string, defs map[string]int, promptTokens, n int) Overhead {
	out := Overhead{Model: model, Requests: n, PromptTokens: promptTokens, Note: "Estimated schema tokens at four bytes per token. Costs project full exposure for the selected number of requests."}
	if promptTokens == 0 {
		out.Note += " Priced at the base rate: no prompt size was measured, so a long-context rate (e.g. Claude Haiku 5.5 above 100K prompt tokens) is not applied."
	}
	for name, t := range defs {
		cached, plain, ok := claudecode.ProjectOverhead(calc, models.SessionProvider(source, model), model, t, promptTokens, n)
		out.Servers = append(out.Servers, Projection{name, t, cached, plain, ok})
	}
	sort.Slice(out.Servers, func(i, j int) bool { return out.Servers[i].Server < out.Servers[j].Server })
	return out
}

func readServers[T any](path string) (map[string]T, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var config struct {
		Servers map[string]T `toml:"mcp_servers" json:"mcpServers"`
	}
	if strings.EqualFold(filepath.Ext(path), ".toml") {
		err = toml.Unmarshal(b, &config)
	} else {
		err = json.Unmarshal(b, &config)
	}
	if err != nil {
		return nil, err
	}
	if len(config.Servers) == 0 {
		return nil, errors.New("MCP config contains no servers")
	}
	return config.Servers, nil
}
