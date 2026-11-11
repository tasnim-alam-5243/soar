// Package integrations embeds the built-in source packs (spec section 10.1).
package integrations

import "embed"

// Packs holds one directory per product: mappings, filters, sources and lists as YAML.
//
//go:embed wazuh/mappings wazuh/filters wazuh/sources wazuh/lists
//go:embed outlook/mappings outlook/sources
//go:embed */manifest.yaml
var Packs embed.FS
