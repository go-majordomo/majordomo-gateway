// Command gateway-mcp is a stdio MCP server that exposes a running Majordomo
// Gateway's usage/cost data as tools for Claude Code / Cowork. It is a thin client
// over the gateway's /api/v1 query API — configure it with the gateway URL and admin
// token via --api-url/--token or GATEWAY_URL/GATEWAY_TOKEN.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	apiURL := flag.String("api-url", envOr("GATEWAY_URL", "http://localhost:6560"), "gateway base URL")
	token := flag.String("token", os.Getenv("GATEWAY_TOKEN"), "gateway admin token")
	flag.Parse()

	c := newClient(*apiURL, *token)
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "majordomo-gateway",
		Title:   "Majordomo Gateway",
		Version: "0.1.0",
	}, nil)

	registerTools(server, c)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintf(os.Stderr, "gateway-mcp: %v\n", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
