// Package gateway embeds the gateway's bundled model catalog and alias table, so
// command-line tools that price usage locally (gateway-cli claude-code) price it
// exactly as the server does, wherever they are installed.
package gateway

import _ "embed"

// ModelCatalog is the bundled model_catalog.json (prices and routes).
//
//go:embed model_catalog.json
var ModelCatalog []byte

// ModelAliases is the bundled model_aliases.json.
//
//go:embed model_aliases.json
var ModelAliases []byte
