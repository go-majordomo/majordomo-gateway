package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/go-majordomo/majordomo-gateway/internal/agentusage"
	"github.com/go-majordomo/majordomo-gateway/internal/claudecode"
)

func runAgentUsage(c *client, source string, args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Printf("Usage: gateway-cli %s <explain|compare|mcp-overhead|import> [options]\n", source)
		return nil
	}
	op := args[0]
	fs := flag.NewFlagSet(source+" "+op, flag.ContinueOnError)
	root := fs.String("store", agentusage.DefaultPath(source), "Codex sessions directory or Cursor state.vscdb or CLI chats directory")
	project := fs.String("project", ".", "project directory for session discovery")
	var id, config, model, beforeConfig, afterConfig, label, key, sinceArg string
	var perCall, dryRun, jsonOut, probe bool
	var watch time.Duration
	var count int
	var measurement, beforeMeasure, afterMeasure string
	meta := metaFlag{}
	switch op {
	case "explain", "mcp-overhead":
		fs.StringVar(&id, "session", "", "session id or unique prefix (default latest in project)")
		fs.StringVar(&config, "mcp-config", "", "list tools from this trusted MCP config and estimate full catalog exposure (JSON or Codex TOML)")
		fs.BoolVar(&jsonOut, "json", false, "print JSON")
		if op == "explain" {
			fs.StringVar(&measurement, "mcp-measurement", "", "use JSON saved by mcp-overhead --probe")
			fs.BoolVar(&perCall, "calls", false, "show each tool call")
		} else {
			fs.BoolVar(&probe, "probe", false, "measure live baseline, individual and combined contexts with the native CLI (uses model calls)")
			fs.IntVar(&count, "requests", 1000, "number of requests to project")
			fs.StringVar(&model, "model", "", "pricing model (required with --mcp-config when no session selected)")
		}
	case "compare":
		fs.StringVar(&beforeMeasure, "before-mcp-measurement", "", "saved before probe JSON")
		fs.StringVar(&afterMeasure, "after-mcp-measurement", "", "saved after probe JSON")
		fs.StringVar(&beforeConfig, "before-mcp-config", "", "trusted MCP config for the before session")
		fs.StringVar(&afterConfig, "after-mcp-config", "", "trusted MCP config for the after session")
		fs.BoolVar(&jsonOut, "json", false, "print JSON")
	case "import":
		fs.StringVar(&id, "session", "", "import a single session by id or prefix")
		fs.StringVar(&key, "key", "", "gateway API key (default $MAJORDOMO_API_KEY)")
		fs.StringVar(&label, "label", source, "run root step name")
		fs.Var(meta, "meta", "repeatable metadata key=value")
		fs.BoolVar(&dryRun, "dry-run", false, "preview import without sending it")
		fs.DurationVar(&watch, "watch", 0, "re-import every interval")
		fs.StringVar(&sinceArg, "since", "", "duration or RFC3339 timestamp")
	default:
		return fmt.Errorf("unknown %s command %q", source, op)
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if op == "compare" && fs.NArg() != 2 {
		return errors.New("compare needs two session ids")
	}
	if op != "compare" && fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if key == "" {
		key = os.Getenv("MAJORDOMO_API_KEY")
	}
	if op == "import" && !dryRun && key == "" {
		return errors.New("--key or MAJORDOMO_API_KEY is required")
	}
	if watch < 0 || count < 0 {
		return errors.New("interval and request count must be non-negative")
	}
	since, err := parseSince(sinceArg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var calc claudecode.Calculator
	if op != "import" {
		p, done, err := claudecode.LocalPricing()
		if err != nil {
			return err
		}
		defer done()
		calc = p
	}
	if probe {
		if config == "" || model == "" {
			return errors.New("--probe requires --model and --mcp-config")
		}
		result, err := agentusage.Probe(ctx, source, model, config)
		if err != nil {
			return err
		}
		deltas := map[string]int{}
		for server, n := range result.Alone {
			deltas[server] = max(0, n)
		}
		projection := agentusage.Project(calc, source, model, deltas, result.Baseline+result.Combined, count)
		projection.Note = "Projected cost for each server measured alone. Negative observed deltas project zero additional cost."
		result.Projection = &projection
		return printJSON(result)
	}
	outputOverhead := func(defs map[string]int, prompt int) error {
		h := agentusage.Project(calc, source, model, defs, prompt, count)
		if jsonOut {
			return printJSON(h)
		}
		fmt.Printf("%s, %d requests. %s\n", model, count, h.Note)
		if len(h.Servers) == 0 {
			fmt.Println("No MCP definitions recorded for this snapshot.")
		}
		for _, s := range h.Servers {
			fmt.Printf("%s  %d tokens/request", s.Server, s.Tokens)
			if s.Priced {
				fmt.Printf("  $%.6f cached / $%.6f uncached", s.Cached, s.Uncached)
			} else {
				fmt.Print("  model unpriced")
			}
			fmt.Println()
		}
		return nil
	}
	if op == "mcp-overhead" && config != "" && model != "" {
		defs, err := agentusage.CatalogDefinitions(ctx, config)
		if err != nil {
			return err
		}
		return outputOverhead(defs, 0)
	}
	once := func() error {
		store, err := agentusage.Open(ctx, source, *root)
		if err != nil {
			return err
		}
		defer store.Close()
		report := func(id, config, measurement string) (agentusage.Report, error) {
			entries, err := store.Find(*project, id, time.Time{})
			if err != nil {
				return agentusage.Report{}, err
			}
			s, err := store.Load(ctx, entries[0])
			if err != nil {
				return agentusage.Report{}, err
			}
			if measurement != "" {
				if config != "" {
					return agentusage.Report{}, errors.New("choose a catalog or a measured probe")
				}
				if err := s.ApplyMeasurement(measurement); err != nil {
					return agentusage.Report{}, err
				}
			}
			if config != "" {
				defs, err := agentusage.CatalogDefinitions(ctx, config)
				if err != nil {
					return agentusage.Report{}, err
				}
				s.ApplyCatalog(defs)
			}
			r, _, err := agentusage.Analyze(s, calc, perCall, label, meta)
			if config != "" {
				r.Note += " Definitions assume all configured tools are loaded."
			}
			return r, err
		}
		if op == "compare" {
			before, err := report(fs.Arg(0), beforeConfig, beforeMeasure)
			if err != nil {
				return err
			}
			after, err := report(fs.Arg(1), afterConfig, afterMeasure)
			if err != nil {
				return err
			}
			cmp := agentusage.Compare(before, after)
			if jsonOut {
				return printJSON(cmp)
			}
			fmt.Println(cmp.Note)
			return printComparison(cmp)
		}
		if op == "explain" {
			r, err := report(id, config, measurement)
			if err != nil {
				return err
			}
			if jsonOut {
				return printJSON(r)
			}
			fmt.Printf("%s session %s. %s\n", source, r.SessionID, r.Note)
			for _, t := range r.Turns {
				printTurnReport(t)
			}
			fmt.Printf("\nSession total: $%.6f (%d input, %d cached, %d output tokens)\n", r.Total, r.Input, r.Cached, r.OutputTokens)
			if len(r.Unpriced) > 0 {
				fmt.Println("Unpriced models:", strings.Join(r.Unpriced, ", "))
			}
			return nil
		}
		entries, err := store.Find(*project, id, since)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			s, err := store.Load(ctx, entry)
			if err != nil {
				return err
			}
			if op == "mcp-overhead" {
				if len(s.Requests) == 0 {
					return errors.New("empty session")
				}
				last := s.Requests[len(s.Requests)-1]
				if model == "" {
					model = last.Model
				}
				defs := last.Definitions
				if config != "" {
					defs, err = agentusage.CatalogDefinitions(ctx, config)
					if err != nil {
						return err
					}
				}
				return outputOverhead(defs, last.InputTokens+last.CacheReadTokens)
			}
			if len(s.Requests) == 0 {
				if id != "" {
					return errors.New("session contains no usage")
				}
				continue
			}
			_, imp, err := agentusage.Analyze(s, nil, false, label, meta)
			if err != nil {
				return err
			}
			if dryRun {
				printImportPreview(&imp)
				if imp.Metadata["usage_basis"] == "estimated" {
					fmt.Println("  Estimated token usage")
				}
				continue
			}
			result, err := importSession(c, source, key, &imp)
			if err != nil {
				return err
			}
			fmt.Printf("%s: %d new requests, %d tool spans, $%.6f (%d unpriced) %s\n", result.TraceID, result.RequestsInserted, result.ToolSpans, result.TotalCost, result.Unpriced, imp.Metadata["usage_basis"])
		}
		return nil
	}
	if err := once(); err != nil || watch == 0 {
		return err
	}
	tick := time.NewTicker(watch)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if err := once(); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		}
	}
}
