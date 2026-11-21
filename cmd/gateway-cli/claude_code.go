package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/go-majordomo/majordomo-gateway/internal/claudecode"
	"github.com/go-majordomo/majordomo-gateway/internal/models"
	"github.com/go-majordomo/majordomo-gateway/internal/sessionclient"
)

const claudeCodeUsage = `Usage: gateway-cli claude-code <command> [options]

Cost of Claude Code sessions, including ones run on a Claude subscription,
priced at API rates. Reads the transcripts Claude Code keeps locally
(~/.claude/projects); prompts, responses and tool content never leave the
machine.

Commands:
  explain        Where each message's cost went: model output, each tool call's
                 result (MCP calls included), MCP tool definitions, subagents and
                 conversation history. Local only.
  compare        How cost changed between two sessions (e.g. an agent run before
                 and after adding a feature), component by component. Local only.
  mcp-overhead   How many tokens each MCP server's tool definitions add to every
                 request. Local only (a few tiny calls on your Claude login).
  import         Send sessions' usage (token counts, model and tool names) to the
                 gateway, as runs you can inspect with 'usage runs' / 'usage run'.
                 Authenticates with a gateway API key (--key or MAJORDOMO_API_KEY).

Run 'gateway-cli claude-code <command> -h' for options.
`

func runClaudeCode(c *client, args []string) error {
	if len(args) < 1 {
		fmt.Print(claudeCodeUsage)
		return nil
	}
	switch args[0] {
	case "import":
		return claudeCodeImport(c, args[1:])
	case "explain":
		return claudeCodeExplain(args[1:])
	case "compare":
		return claudeCodeCompare(args[1:])
	case "mcp-overhead":
		return claudeCodeMCPOverhead(args[1:])
	case "help", "-h", "--help":
		fmt.Print(claudeCodeUsage)
		return nil
	default:
		return fmt.Errorf("unknown claude-code command %q", args[0])
	}
}

// metaFlag collects repeatable key=value flags.
type metaFlag map[string]string

func (m metaFlag) String() string { return "" }
func (m metaFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("expected key=value, got %q", v)
	}
	m[k] = val
	return nil
}

func parseSince(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("--since must be a duration (e.g. 24h) or RFC3339 time")
	}
	return t, nil
}

// ── import ────────────────────────────────────────────────────────────────────

func claudeCodeImport(c *client, args []string) error {
	fs := flag.NewFlagSet("claude-code import", flag.ContinueOnError)
	project := fs.String("project", ".", "only sessions started inside this directory")
	session := fs.String("session", "", "only this session (id or unique id prefix, any project)")
	sinceFlag := fs.String("since", "", "only sessions active since (duration like 24h, or RFC3339)")
	label := fs.String("label", claudecode.DefaultLabel, "top-level step name of imported runs")
	key := fs.String("key", "", "gateway API key (default $MAJORDOMO_API_KEY)")
	claudeDir := fs.String("claude-dir", claudecode.DefaultProjectsDir(), "Claude Code transcripts directory")
	watch := fs.Duration("watch", 0, "keep running and import again every interval (e.g. 30s)")
	dryRun := fs.Bool("dry-run", false, "print what would be sent, send nothing")
	meta := metaFlag{}
	fs.Var(meta, "meta", "metadata to tag the runs with, key=value (repeatable, e.g. --meta variant=baseline)")
	if err := fs.Parse(args); err != nil {
		return helpOK(err)
	}
	if *key == "" {
		*key = os.Getenv("MAJORDOMO_API_KEY")
	}
	if *key == "" && !*dryRun {
		return errors.New("--key or MAJORDOMO_API_KEY is required")
	}
	since, err := parseSince(*sinceFlag)
	if err != nil {
		return err
	}

	importOnce := func() error {
		var paths []string
		if *session != "" {
			p, err := claudecode.ResolveSession(*claudeDir, *project, *session)
			if err != nil {
				return err
			}
			paths = []string{p}
		} else {
			if paths, err = claudecode.FindSessions(*claudeDir, *project, since); err != nil {
				return err
			}
			if len(paths) == 0 {
				return errors.New("no Claude Code session found in this project (try --project or --session)")
			}
		}
		for _, p := range paths {
			s, err := claudecode.LoadSession(*claudeDir, p)
			if err != nil {
				return err
			}
			imp := claudecode.BuildImport(s, *label, meta)
			if len(imp.Requests) == 0 {
				continue
			}
			if *dryRun {
				printImportPreview(&imp)
				continue
			}
			res, err := importSession(c, *key, &imp)
			if err != nil {
				return fmt.Errorf("import session %s: %w", s.ID, err)
			}
			fmt.Printf("%s  %s  %d requests (%d new), %d tool calls, $%.6f (tools $%.6f)",
				time.Now().Format("15:04:05"), res.TraceID, res.Requests, res.RequestsInserted,
				res.ToolSpans, res.TotalCost, res.ToolCost)
			if res.Unpriced > 0 {
				fmt.Printf(", %d unpriced", res.Unpriced)
			}
			fmt.Println()
		}
		return nil
	}

	if err := importOnce(); err != nil || *watch == 0 {
		if err == nil && !*dryRun {
			fmt.Println("\nInspect with: gateway-cli usage runs   /   gateway-cli usage run <trace id>")
		}
		return err
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	ticker := time.NewTicker(*watch)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return importOnce()
		case <-ticker.C:
			if err := importOnce(); err != nil {
				fmt.Fprintln(os.Stderr, "import failed (will retry):", err)
			}
		}
	}
}

func printImportPreview(imp *models.ClaudeCodeImport) {
	steps := map[string]int{}
	var order []string
	for _, r := range imp.Requests {
		if steps[r.SpanPath] == 0 {
			order = append(order, r.SpanPath)
		}
		steps[r.SpanPath]++
	}
	fmt.Printf("Session %s: %d requests, %d tool calls (usage numbers only)\n", imp.SessionID, len(imp.Requests), len(imp.ToolSpans))
	for _, s := range order {
		fmt.Printf("  %-50s %d requests\n", s, steps[s])
	}
}

// importSession posts a session to the gateway, authenticated with a gateway API
// key (the import endpoint uses the caller's key, not the admin token).
func importSession(c *client, key string, imp *models.ClaudeCodeImport) (*models.ClaudeCodeImportResult, error) {
	return sessionclient.Import(c.http, c.baseURL, "claude-code", key, imp)
}

// ── explain / compare ─────────────────────────────────────────────────────────

// buildReport loads and prices one session, measuring MCP tool definitions
// first when an MCP config is given.
func buildReport(claudeDir, project, session, mcpConfig string, perCall bool) (claudecode.Report, error) {
	path, err := claudecode.ResolveSession(claudeDir, project, session)
	if err != nil {
		return claudecode.Report{}, err
	}
	s, err := claudecode.LoadSession(claudeDir, path)
	if err != nil {
		return claudecode.Report{}, err
	}
	calc, done, err := claudecode.LocalPricing()
	if err != nil {
		return claudecode.Report{}, err
	}
	defer done()

	return claudecode.SessionReport(calc, s, mcpConfig, perCall)
}

func claudeCodeExplain(args []string) error {
	fs := flag.NewFlagSet("claude-code explain", flag.ContinueOnError)
	project := fs.String("project", ".", "with no --session: the most recent session started in this directory")
	session := fs.String("session", "", "session id or unique id prefix (any project)")
	claudeDir := fs.String("claude-dir", claudecode.DefaultProjectsDir(), "Claude Code transcripts directory")
	mcpConfig := fs.String("mcp-config", "", "MCP config the session used; measures and separates out tool-definition cost")
	calls := fs.Bool("calls", false, "list every tool call instead of grouping by tool")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		return helpOK(err)
	}
	r, err := buildReport(*claudeDir, *project, *session, *mcpConfig, *calls)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(r)
	}

	fmt.Printf("Session %s: %d turn(s). Costs are API-equivalent (what this would cost on the API).\n", r.SessionID, len(r.Turns))
	for _, t := range r.Turns {
		printTurnReport(t)
	}
	fmt.Printf("\nSession total: $%.6f\n", r.Total)
	if len(r.Unpriced) > 0 {
		fmt.Printf("No price found for: %s (counted as $0).\n", strings.Join(r.Unpriced, ", "))
	}
	return nil
}

func printTurnReport(t claudecode.TurnReport) {
	fmt.Printf("\nTurn %d  %s  %q\n", t.Number, t.At.Local().Format("15:04:05"), t.Prompt)
	fmt.Printf("  %d request(s), total $%.6f\n", t.Main.Requests, t.Total)
	if t.Warning != "" {
		fmt.Printf("  warning: %s\n", t.Warning)
	}
	line := func(indent, label string, v float64) {
		pct := ""
		if t.Total > 0 {
			pct = fmt.Sprintf("%3.0f%%", 100*v/t.Total)
		}
		fmt.Printf("%s%-58s $%.6f %s\n", indent, truncate(label, 58), v, pct)
	}
	printPart := func(indent string, p claudecode.PartReport, main bool) {
		line(indent, "Model output (replies, reasoning, tool-call arguments)", p.Output)
		if len(p.Tools) > 0 {
			line(indent, "Tool results", p.ToolsTotal)
			for _, tl := range p.Tools {
				label := fmt.Sprintf("%s ×%d  %d tok", tl.Tool, tl.Calls, tl.Tokens)
				switch {
				case tl.Earlier:
					label += "  (earlier, re-read)"
				case tl.Rereads > 0:
					label += fmt.Sprintf("  (+%d re-reads)", tl.Rereads)
				}
				line(indent+"  ", label, tl.Cost)
			}
		}
		if len(p.Definitions) > 0 {
			line(indent, "MCP tool definitions", p.DefinitionsTotal)
			names := make([]string, 0, len(p.Definitions))
			for n := range p.Definitions {
				names = append(names, n)
			}
			sort.Slice(names, func(i, j int) bool { return p.Definitions[names[i]] > p.Definitions[names[j]] })
			for _, n := range names {
				line(indent+"  ", n, p.Definitions[n])
			}
			line(indent, "System prompt, conversation history", p.Rest)
		} else if main {
			line(indent, "System prompt, tool definitions, conversation history", p.Rest)
		} else {
			line(indent, "context", p.Rest)
		}
	}
	printPart("  ", t.Main, true)
	if len(t.Subagents) == 0 {
		return
	}
	subTotal := 0.0
	for _, s := range t.Subagents {
		subTotal += s.Total
	}
	line("  ", "Subagents", subTotal)
	for _, s := range t.Subagents {
		line("    ", s.Name, s.Total)
		printPart("      ", s.PartReport, false)
	}
}

func claudeCodeCompare(args []string) error {
	fs := flag.NewFlagSet("claude-code compare", flag.ContinueOnError)
	claudeDir := fs.String("claude-dir", claudecode.DefaultProjectsDir(), "Claude Code transcripts directory")
	beforeMCP := fs.String("before-mcp-config", "", "MCP config the first session used (separates definition cost)")
	afterMCP := fs.String("after-mcp-config", "", "MCP config the second session used")
	asJSON := fs.Bool("json", false, "print the comparison as JSON")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: gateway-cli claude-code compare [options] <before session id> <after session id>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return helpOK(err)
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return errors.New("compare needs two session ids (or unique prefixes)")
	}
	before, err := buildReport(*claudeDir, ".", fs.Arg(0), *beforeMCP, false)
	if err != nil {
		return fmt.Errorf("before: %w", err)
	}
	after, err := buildReport(*claudeDir, ".", fs.Arg(1), *afterMCP, false)
	if err != nil {
		return fmt.Errorf("after: %w", err)
	}
	cmp := claudecode.Compare(before, after)
	if *asJSON {
		return printJSON(cmp)
	}

	fmt.Printf("Before %s: $%.6f (%d requests)\n", short(cmp.Before), cmp.BeforeTotal, cmp.BeforeRequests)
	fmt.Printf("After  %s: $%.6f (%d requests)\n", short(cmp.After), cmp.AfterTotal, cmp.AfterRequests)
	fmt.Printf("Change: %+.6f (%+.0f%%)\n\n", cmp.Delta, cmp.DeltaPercent)
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "COMPONENT\tBEFORE\tAFTER\tCHANGE"); err != nil {
		return err
	}
	for _, l := range cmp.Lines {
		if _, err := fmt.Fprintf(w, "%s\t$%.6f\t$%.6f\t%+.6f\n", truncate(l.Component, 50), l.Before, l.After, l.Delta); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Println("\nAPI-equivalent costs. Components don't overlap and sum to each total.")
	return nil
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// ── mcp-overhead ──────────────────────────────────────────────────────────────

func claudeCodeMCPOverhead(args []string) error {
	fs := flag.NewFlagSet("claude-code mcp-overhead", flag.ContinueOnError)
	config := fs.String("mcp-config", ".mcp.json", "MCP config file ({\"mcpServers\": {...}})")
	model := fs.String("model", "haiku", "model to measure with")
	requests := fs.Int("requests", 1000, "number of requests to project cost for")
	if err := fs.Parse(args); err != nil {
		return helpOK(err)
	}
	m, err := claudecode.MeasureMCP(*model, *config)
	if err != nil {
		return err
	}
	calc, done, err := claudecode.LocalPricing()
	if err != nil {
		return err
	}
	defer done()

	// Cost of tokens per request over the projection, cached or uncached, at
	// the price tier of the measured request (baseline plus every server).
	cost := func(tokens int) (string, string) {
		cached, plain, ok := claudecode.ProjectOverhead(calc, "anthropic", m.ModelID, tokens, m.Baseline+m.Combined, *requests)
		if !ok {
			return "-", "-"
		}
		return fmt.Sprintf("$%.4f", cached), fmt.Sprintf("$%.4f", plain)
	}

	fmt.Printf("Model %s, baseline context %d tokens (no MCP servers).\n\n", m.ModelID, m.Baseline)
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	if _, err := fmt.Fprintf(w, "MCP SERVER\tTOKENS ALONE\tTOKENS ALONGSIDE OTHERS\tPER %d REQUESTS (CACHED)\tPER %d REQUESTS (UNCACHED)\n", *requests, *requests); err != nil {
		return err
	}
	for _, name := range m.Names {
		cached, plain := cost(m.Alone[name])
		if _, err := fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%s\n", name, m.Alone[name], m.Alongside(name), cached, plain); err != nil {
			return err
		}
	}
	if len(m.Names) > 1 {
		cached, plain := cost(m.Combined)
		if _, err := fmt.Fprintf(w, "ALL %d TOGETHER\t%d\t\t%s\t%s\n", len(m.Names), m.Combined, cached, plain); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if m.Shared > 0 {
		fmt.Printf("\nShared MCP preamble: ~%d tokens, sent once however many servers are connected.\n", m.Shared)
	}
	fmt.Println("Cached: re-read from the prompt cache (typical in agent loops). Uncached: first request, or no caching.")
	return nil
}

// helpOK treats -h (the flag package has already printed the usage) as success.
func helpOK(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}
