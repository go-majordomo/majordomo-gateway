package main

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
)

type metadataKey struct {
	MajordomoAPIKeyID string `json:"majordomo_api_key_id"`
	APIKeyName        string `json:"api_key_name"`
	KeyName           string `json:"key_name"`
	IsActive          bool   `json:"is_active"`
	ApproxCardinality int64  `json:"approx_cardinality"`
	RequestCount      int64  `json:"request_count"`
}

func runMetadata(c *client, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: gateway-cli metadata <list|activate|deactivate>")
	}
	switch args[0] {
	case "list":
		return runMetadataList(c)
	case "activate":
		return runMetadataToggle(c, "activate", args[1:])
	case "deactivate":
		return runMetadataToggle(c, "deactivate", args[1:])
	default:
		return fmt.Errorf("unknown metadata subcommand: %s", args[0])
	}
}

func runMetadataList(c *client) error {
	var keys []metadataKey
	if err := c.get("/api/v1/metadata", &keys); err != nil {
		return err
	}
	if len(keys) == 0 {
		fmt.Println("No metadata keys discovered yet. Send requests with X-Majordomo-<key> headers first.")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "API KEY\tMETADATA KEY\t~CARDINALITY\tINDEXED\tREQUESTS\tAPI KEY ID")
	for _, k := range keys {
		fmt.Fprintf(w, "%s\t%s\t%d\t%t\t%d\t%s\n",
			k.APIKeyName, k.KeyName, k.ApproxCardinality, k.IsActive, k.RequestCount, k.MajordomoAPIKeyID)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Println("\nOnly INDEXED keys are queryable/groupable. Activate low-cardinality keys;")
	fmt.Println("leave high-cardinality keys (e.g. user_id) un-indexed to keep the index bounded.")
	return nil
}

func runMetadataToggle(c *client, action string, args []string) error {
	fs := flag.NewFlagSet("metadata "+action, flag.ContinueOnError)
	apiKey := fs.String("api-key", "", "the API key id the metadata key belongs to — required")
	if err := fs.Parse(args); err != nil {
		return err
	}
	keyName := fs.Arg(0)
	if *apiKey == "" || keyName == "" {
		return fmt.Errorf("usage: gateway-cli metadata %s --api-key <id> <metadata-key>", action)
	}
	body := map[string]string{"api_key_id": *apiKey, "key_name": keyName}
	var resp map[string]any
	if err := c.post("/api/v1/metadata/"+action, body, &resp); err != nil {
		return err
	}
	fmt.Printf("%s %q for API key %s (reindexing existing rows in the background)\n", action, keyName, *apiKey)
	return nil
}
