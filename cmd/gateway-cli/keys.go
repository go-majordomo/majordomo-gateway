package main

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"
)

type apiKey struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Description  *string    `json:"description,omitempty"`
	IsActive     bool       `json:"is_active"`
	RequestCount int64      `json:"request_count"`
	CreatedAt    time.Time  `json:"created_at"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
}

type createKeyResponse struct {
	apiKey
	Key string `json:"key"`
}

func runKeys(c *client, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: gateway-cli keys <create|list|revoke>")
	}
	switch args[0] {
	case "create":
		return runKeysCreate(c, args[1:])
	case "list":
		return runKeysList(c)
	case "revoke":
		return runKeysRevoke(c, args[1:])
	default:
		return fmt.Errorf("unknown keys subcommand: %s", args[0])
	}
}

func runKeysCreate(c *client, args []string) error {
	fs := flag.NewFlagSet("keys create", flag.ContinueOnError)
	name := fs.String("name", "", "key name (e.g. billing-service) — required")
	description := fs.String("description", "", "optional description")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("--name is required")
	}

	body := map[string]any{"name": *name}
	if *description != "" {
		body["description"] = *description
	}

	var resp createKeyResponse
	if err := c.post("/api/v1/keys", body, &resp); err != nil {
		return err
	}

	fmt.Printf("Created API key %q (%s)\n", resp.Name, resp.ID)
	fmt.Printf("\n  %s\n\n", resp.Key)
	fmt.Println("This is the only time the key is shown — store it now.")
	return nil
}

func runKeysList(c *client) error {
	var keys []apiKey
	if err := c.get("/api/v1/keys", &keys); err != nil {
		return err
	}
	if len(keys) == 0 {
		fmt.Println("No API keys.")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tACTIVE\tREQUESTS\tCREATED\tLAST USED")
	for _, k := range keys {
		last := "never"
		if k.LastUsedAt != nil {
			last = k.LastUsedAt.Format("2006-01-02")
		}
		fmt.Fprintf(w, "%s\t%s\t%t\t%d\t%s\t%s\n",
			k.ID, k.Name, k.IsActive, k.RequestCount, k.CreatedAt.Format("2006-01-02"), last)
	}
	return w.Flush()
}

func runKeysRevoke(c *client, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: gateway-cli keys revoke <id>")
	}
	if err := c.delete("/api/v1/keys/" + args[0]); err != nil {
		return err
	}
	fmt.Printf("Revoked API key %s\n", args[0])
	return nil
}
