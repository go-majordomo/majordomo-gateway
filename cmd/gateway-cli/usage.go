package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

// metadataFlag collects repeatable --metadata key=value pairs.
type metadataFlag []map[string]string

func (m *metadataFlag) String() string { return "" }
func (m *metadataFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" || val == "" {
		return fmt.Errorf("metadata must be key=value")
	}
	*m = append(*m, map[string]string{"key": k, "value": val})
	return nil
}

// usageFlags holds the parsed common filter flags plus the output mode.
type usageFlags struct {
	body map[string]any
	json bool
}

// parseUsageFlags registers and parses the shared usage filter flags, returning the
// request body for the POST usage endpoints.
func parseUsageFlags(name string, args []string) (*usageFlags, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	preset := fs.String("preset", "", "time window: 7d|30d|90d (default 30d)")
	start := fs.String("start", "", "start date YYYY-MM-DD")
	end := fs.String("end", "", "end date YYYY-MM-DD")
	provider := fs.String("provider", "", "filter by provider")
	model := fs.String("model", "", "filter by model")
	apiKey := fs.String("api-key", "", "filter by API key id")
	status := fs.String("status", "", "filter by status class: error|success")
	limit := fs.Int("limit", 0, "max rows (requests/runs)")
	offset := fs.Int("offset", 0, "row offset (requests/runs)")
	asJSON := fs.Bool("json", false, "print raw JSON instead of a table")
	var metadata metadataFlag
	fs.Var(&metadata, "metadata", "metadata filter key=value (repeatable, max 2)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	body := map[string]any{}
	setIf := func(k, v string) {
		if v != "" {
			body[k] = v
		}
	}
	setIf("preset", *preset)
	setIf("start", *start)
	setIf("end", *end)
	setIf("provider", *provider)
	setIf("model", *model)
	setIf("api_key_id", *apiKey)
	setIf("status_class", *status)
	if *limit > 0 {
		body["limit"] = *limit
	}
	if *offset > 0 {
		body["offset"] = *offset
	}
	if len(metadata) > 0 {
		body["metadata_filters"] = metadata
	}

	return &usageFlags{body: body, json: *asJSON}, nil
}

func runUsage(c *client, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: gateway-cli usage <summary|models|providers|keys|errors|latency|requests|runs|run>")
	}
	sub := args[0]
	rest := args[1:]

	// `run <traceId>` is a GET with a path param, not a filtered POST.
	if sub == "run" {
		if len(rest) < 1 {
			return fmt.Errorf("usage: gateway-cli usage run <traceId>")
		}
		return runUsageRunTree(c, rest[0], contains(rest, "--json"))
	}

	// `request <id> [--body]` fetches one request's detail, or its archived bodies.
	if sub == "request" {
		if len(rest) < 1 {
			return fmt.Errorf("usage: gateway-cli usage request <id> [--body]")
		}
		path := "/api/v1/usage/requests/" + rest[0]
		if contains(rest, "--body") {
			path += "/body"
		}
		var out json.RawMessage
		if err := c.get(path, &out); err != nil {
			return err
		}
		return printJSON(out)
	}

	uf, err := parseUsageFlags("usage "+sub, rest)
	if err != nil {
		return err
	}

	switch sub {
	case "summary":
		return usageSummary(c, uf)
	case "models":
		return usageModels(c, uf)
	case "providers":
		return usageProviders(c, uf)
	case "keys":
		return usageAPIKeys(c, uf)
	case "requests":
		return usageRequests(c, uf)
	case "runs":
		return usageRuns(c, uf)
	case "errors":
		return usageRawJSON(c, "/api/v1/usage/errors", uf)
	case "latency":
		return usageRawJSON(c, "/api/v1/usage/latency", uf)
	case "daily":
		return usageRawJSON(c, "/api/v1/usage/daily", uf)
	default:
		return fmt.Errorf("unknown usage subcommand: %s", sub)
	}
}

func usageSummary(c *client, uf *usageFlags) error {
	var s struct {
		TotalRequests            int64   `json:"total_requests"`
		TotalInputTokens         int64   `json:"total_input_tokens"`
		TotalOutputTokens        int64   `json:"total_output_tokens"`
		TotalCachedTokens        int64   `json:"total_cached_tokens"`
		TotalCacheCreationTokens int64   `json:"total_cache_creation_tokens"`
		TotalCost                float64 `json:"total_cost"`
	}
	if err := c.post("/api/v1/usage/summary", uf.body, &s); err != nil {
		return err
	}
	if uf.json {
		return printJSON(s)
	}
	fmt.Printf("Requests:       %d\n", s.TotalRequests)
	fmt.Printf("Input tokens:   %d\n", s.TotalInputTokens)
	fmt.Printf("Output tokens:  %d\n", s.TotalOutputTokens)
	fmt.Printf("Cached tokens:  %d\n", s.TotalCachedTokens)
	fmt.Printf("Total cost:     $%.4f\n", s.TotalCost)
	return nil
}

func usageModels(c *client, uf *usageFlags) error {
	var rows []struct {
		Provider     string  `json:"provider"`
		Model        string  `json:"model"`
		RequestCount int64   `json:"request_count"`
		InputTokens  int64   `json:"input_tokens"`
		OutputTokens int64   `json:"output_tokens"`
		TotalCost    float64 `json:"total_cost"`
	}
	if err := c.post("/api/v1/usage/models", uf.body, &rows); err != nil {
		return err
	}
	if uf.json {
		return printJSON(rows)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "PROVIDER\tMODEL\tREQUESTS\tINPUT\tOUTPUT\tCOST")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t$%.4f\n", r.Provider, r.Model, r.RequestCount, r.InputTokens, r.OutputTokens, r.TotalCost)
	}
	return w.Flush()
}

func usageProviders(c *client, uf *usageFlags) error {
	var rows []struct {
		Provider     string  `json:"provider"`
		RequestCount int64   `json:"request_count"`
		TotalCost    float64 `json:"total_cost"`
	}
	if err := c.post("/api/v1/usage/providers", uf.body, &rows); err != nil {
		return err
	}
	if uf.json {
		return printJSON(rows)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "PROVIDER\tREQUESTS\tCOST")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%d\t$%.4f\n", r.Provider, r.RequestCount, r.TotalCost)
	}
	return w.Flush()
}

func usageAPIKeys(c *client, uf *usageFlags) error {
	var rows []struct {
		APIKeyName   string  `json:"api_key_name"`
		RequestCount int64   `json:"request_count"`
		TotalCost    float64 `json:"total_cost"`
	}
	if err := c.post("/api/v1/usage/api-keys", uf.body, &rows); err != nil {
		return err
	}
	if uf.json {
		return printJSON(rows)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "API KEY\tREQUESTS\tCOST")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%d\t$%.4f\n", r.APIKeyName, r.RequestCount, r.TotalCost)
	}
	return w.Flush()
}

func usageRequests(c *client, uf *usageFlags) error {
	var resp struct {
		Requests []struct {
			ID          string    `json:"id"`
			Provider    string    `json:"provider"`
			Model       string    `json:"model"`
			RequestedAt time.Time `json:"requested_at"`
			StatusCode  int       `json:"status_code"`
			TotalCost   float64   `json:"total_cost"`
		} `json:"requests"`
		NumRecords int `json:"numRecords"`
	}
	if err := c.post("/api/v1/usage/requests", uf.body, &resp); err != nil {
		return err
	}
	if uf.json {
		return printJSON(resp)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "TIME\tPROVIDER\tMODEL\tSTATUS\tCOST\tID")
	for _, r := range resp.Requests {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t$%.4f\t%s\n",
			r.RequestedAt.Format("2006-01-02 15:04"), r.Provider, r.Model, r.StatusCode, r.TotalCost, r.ID)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Printf("\n%d total\n", resp.NumRecords)
	return nil
}

func usageRuns(c *client, uf *usageFlags) error {
	var resp struct {
		Runs []struct {
			TraceID      string    `json:"trace_id"`
			Label        string    `json:"label"`
			RequestCount int64     `json:"request_count"`
			TotalCost    float64   `json:"total_cost"`
			StartedAt    time.Time `json:"started_at"`
		} `json:"runs"`
		NumRecords int `json:"numRecords"`
	}
	if err := c.post("/api/v1/usage/runs", uf.body, &resp); err != nil {
		return err
	}
	if uf.json {
		return printJSON(resp)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "STARTED\tLABEL\tCALLS\tCOST\tTRACE ID")
	for _, r := range resp.Runs {
		fmt.Fprintf(w, "%s\t%s\t%d\t$%.4f\t%s\n",
			r.StartedAt.Format("2006-01-02 15:04"), r.Label, r.RequestCount, r.TotalCost, r.TraceID)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Printf("\n%d total — inspect one with: gateway-cli usage run <traceId>\n", resp.NumRecords)
	return nil
}

// runNode mirrors models.RunNode for rendering the waterfall.
type runNode struct {
	Name         string     `json:"name"`
	Kind         string     `json:"kind"`
	Model        *string    `json:"model,omitempty"`
	TotalCost    float64    `json:"total_cost"`
	RequestCount int64      `json:"request_count"`
	Children     []*runNode `json:"children"`
}

func runUsageRunTree(c *client, traceID string, asJSON bool) error {
	var tree struct {
		TraceID      string   `json:"trace_id"`
		Root         *runNode `json:"root"`
		TotalCost    float64  `json:"total_cost"`
		RequestCount int64    `json:"request_count"`
	}
	if err := c.get("/api/v1/usage/runs/"+traceID, &tree); err != nil {
		return err
	}
	if asJSON {
		return printJSON(tree)
	}
	fmt.Printf("Run %s — %d calls, $%.4f total\n\n", tree.TraceID, tree.RequestCount, tree.TotalCost)
	if tree.Root != nil {
		printRunNode(tree.Root, 0)
	}
	return nil
}

func printRunNode(n *runNode, depth int) {
	indent := strings.Repeat("  ", depth)
	label := n.Name
	if n.Kind == "llm" && n.Model != nil {
		label = fmt.Sprintf("%s (%s)", n.Name, *n.Model)
	}
	fmt.Printf("%s%-40s $%.4f\n", indent, truncate(label, 40), n.TotalCost)
	for _, child := range n.Children {
		printRunNode(child, depth+1)
	}
}

func usageRawJSON(c *client, path string, uf *usageFlags) error {
	var out json.RawMessage
	if err := c.post(path, uf.body, &out); err != nil {
		return err
	}
	return printJSON(out)
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func contains(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}
