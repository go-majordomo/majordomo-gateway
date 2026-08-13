package main

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"
)

type providerKeyInfo struct {
	Provider  string    `json:"provider"`
	CreatedAt time.Time `json:"created_at"`
}

func runProviderKeys(c *client, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: gateway-cli provider-keys <add|list|remove>")
	}
	switch args[0] {
	case "add":
		return runProviderKeysAdd(c, args[1:])
	case "list":
		return runProviderKeysList(c)
	case "remove":
		return runProviderKeysRemove(c, args[1:])
	default:
		return fmt.Errorf("unknown provider-keys subcommand: %s", args[0])
	}
}

func runProviderKeysAdd(c *client, args []string) error {
	fs := flag.NewFlagSet("provider-keys add", flag.ContinueOnError)
	prov := fs.String("provider", "", "provider name (e.g. fireworks) — required")
	key := fs.String("key", "", "provider API key (plaintext) — required")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *prov == "" || *key == "" {
		return fmt.Errorf("--provider and --key are required")
	}

	var resp providerKeyInfo
	if err := c.post("/api/v1/provider-keys", map[string]any{"provider": *prov, "key": *key}, &resp); err != nil {
		return err
	}
	fmt.Printf("Stored provider key for %q\n", resp.Provider)
	return nil
}

func runProviderKeysList(c *client) error {
	var keys []providerKeyInfo
	if err := c.get("/api/v1/provider-keys", &keys); err != nil {
		return err
	}
	if len(keys) == 0 {
		fmt.Println("No provider keys.")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "PROVIDER\tCREATED")
	for _, k := range keys {
		fmt.Fprintf(w, "%s\t%s\n", k.Provider, k.CreatedAt.Format("2006-01-02"))
	}
	return w.Flush()
}

func runProviderKeysRemove(c *client, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: gateway-cli provider-keys remove <provider>")
	}
	if err := c.delete("/api/v1/provider-keys/" + args[0]); err != nil {
		return err
	}
	fmt.Printf("Removed provider key for %s\n", args[0])
	return nil
}
