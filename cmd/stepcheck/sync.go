package main

import (
	"fmt"
	"net/http"
	"os"

	"gopkg.in/yaml.v3"
)

const pullPack = "integrations/wazuh/sources/wazuh-pull.yaml"

// pullPackConfig reads the "config" block of the built-in wazuh-pull source pack.
func pullPackConfig(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Config map[string]any `yaml:"config"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(doc.Config) == 0 {
		return nil, fmt.Errorf("%s has no config block", path)
	}
	return doc.Config, nil
}

// syncPullSource copies the pack's current config onto the existing wazuh-pull source.
// `make seed` never overwrites a source that already exists, so a source seeded by an
// older build keeps its old paging settings until this runs. Enabled, schedule and the
// indexer connection are left as they are.
func syncPullSource(c *client) error {
	cfg, err := pullPackConfig(pullPack)
	if err != nil {
		return err
	}
	id, err := c.sourceID("poll_http", "wazuh-pull")
	if err != nil {
		return err
	}
	before, err := c.apiWant(http.MethodGet, "/sources/"+id, nil, 200)
	if err != nil {
		return err
	}
	after, err := c.apiWant(http.MethodPatch, "/sources/"+id, map[string]any{"config": cfg}, 200)
	if err != nil {
		return err
	}
	fmt.Printf("wazuh-pull (%s) config updated from %s\n", id, pullPack)
	for _, k := range []string{"key_path", "overlap_pages", "max_pages_per_cycle"} {
		fmt.Printf("  %-20s %v -> %v\n", k, orNone(at(before.Body, "config", k)), orNone(at(after.Body, "config", k)))
	}
	fmt.Printf("  %-20s %v (unchanged)\n", "enabled", at(after.Body, "enabled"))
	return nil
}

func orNone(v any) any {
	if v == nil {
		return "(not set)"
	}
	return v
}
