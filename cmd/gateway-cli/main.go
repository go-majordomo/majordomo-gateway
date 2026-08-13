// Command gateway-cli is a thin HTTP client for a running Majordomo Gateway. It
// manages API keys and queries usage/cost data over the gateway's /api/v1 admin API.
package main

import (
	"fmt"
	"os"
)

const defaultAPIURL = "http://localhost:6560"

func main() {
	// A leading `--api-url <url>` / `--token <tok>` may precede the subcommand.
	args := os.Args[1:]
	apiURL := envOr("GATEWAY_URL", defaultAPIURL)
	token := os.Getenv("GATEWAY_TOKEN")

	for len(args) >= 2 {
		switch args[0] {
		case "--api-url":
			apiURL, args = args[1], args[2:]
		case "--token":
			token, args = args[1], args[2:]
		default:
			goto dispatch
		}
	}

dispatch:
	if len(args) < 1 {
		printUsage()
		os.Exit(1)
	}

	c := newClient(apiURL, token)

	var err error
	switch args[0] {
	case "keys":
		err = runKeys(c, args[1:])
	case "provider-keys":
		err = runProviderKeys(c, args[1:])
	case "metadata":
		err = runMetadata(c, args[1:])
	case "usage":
		err = runUsage(c, args[1:])
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", args[0])
		printUsage()
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func printUsage() {
	fmt.Print(`Usage: gateway-cli [--api-url <url>] [--token <token>] <command> [options]

Global flags (or env GATEWAY_URL / GATEWAY_TOKEN):
  --api-url <url>    Gateway base URL (default http://localhost:6560)
  --token <token>    Admin token for the /api/v1 API

Commands:
  keys create --name <name> [--description <text>]   Mint a new API key (shown once)
  keys list                                          List API keys
  keys revoke <id>                                   Revoke an API key

  provider-keys add --provider <p> --key <k>         Store an encrypted upstream provider key
  provider-keys list                                 List stored provider keys (no key material)
  provider-keys remove <provider>                    Remove a stored provider key

  metadata list                                      Discovered metadata keys + cardinality
  metadata activate --api-key <id> <key>             Index a metadata key (makes it queryable)
  metadata deactivate --api-key <id> <key>           Stop indexing a metadata key

  usage summary      Total requests / tokens / cost
  usage models       Cost broken down by provider+model
  usage providers    Cost broken down by provider
  usage keys         Cost broken down by API key
  usage errors       Error-rate summary
  usage latency      Latency percentiles
  usage requests     Recent request log rows
  usage runs         Agent runs (grouped by trace id)
  usage run <traceId>  Render one agent run's cost waterfall
  usage request <id> [--body]  One request's detail, or its archived bodies (--body)

Usage filters (for the usage subcommands, except run):
  --preset 7d|30d|90d     Time window (default 30d)
  --start / --end         Explicit YYYY-MM-DD range (overrides preset)
  --provider <name>       Filter by provider
  --model <name>          Filter by model
  --api-key <uuid>        Filter by API key id
  --status error|success  Filter by status class
  --metadata key=value    Filter by indexed metadata (repeatable, max 2)
  --limit / --offset      Pagination (requests/runs)
  --json                  Print raw JSON instead of a table
`)
}
