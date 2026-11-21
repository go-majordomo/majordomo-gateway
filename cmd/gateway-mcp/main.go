// Command gateway-mcp is a stdio MCP server that exposes a running Majordomo
// Gateway's usage/cost data as tools for Claude Code / Cowork. It is a thin client
// over the gateway's /api/v1 query API — configure it with the gateway URL and admin
// token via --api-url/--token or GATEWAY_URL/GATEWAY_TOKEN.
//
// It also answers cost questions about Claude Code sessions from the transcripts
// Claude Code keeps on this machine (claude_code_explain / claude_code_compare),
// with no gateway needed, and imports them into the gateway (claude_code_import,
// which needs a gateway API key via --api-key or MAJORDOMO_API_KEY).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() { os.Exit(run()) }

func run() int {
	apiURL := flag.String("api-url", envOr("GATEWAY_URL", "http://localhost:6560"), "gateway base URL")
	// Secrets default to empty and fall back to the environment after parsing,
	// so -h never prints them.
	token := flag.String("token", "", "gateway admin token (default $GATEWAY_TOKEN)")
	apiKey := flag.String("api-key", "", "gateway API key, for claude_code_import (default $MAJORDOMO_API_KEY)")
	flag.Parse()
	if *token == "" {
		*token = os.Getenv("GATEWAY_TOKEN")
	}
	if *apiKey == "" {
		*apiKey = os.Getenv("MAJORDOMO_API_KEY")
	}

	c := newClient(*apiURL, *token)
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "majordomo-gateway",
		Title:   "Majordomo Gateway",
		Version: "0.1.0",
	}, nil)

	registerTools(server, c)
	tools := &claudeCodeTools{c: c, apiKey: *apiKey}
	defer func() {
		if tools.cleanup != nil {
			tools.cleanup()
		}
	}()
	tools.registerClaudeCodeTools(server)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintf(os.Stderr, "gateway-mcp: %v\n", err)
		return 1
	}
	return 0
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
