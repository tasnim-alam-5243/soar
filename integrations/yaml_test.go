package integrations

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Every YAML file of every integration parses, including files that only the Python
// actions read (dfir_iris/actions/endpoints.yaml once had unquoted "{case_id}" paths
// inside flow mappings, which broke every IRIS action at import time).
func TestEveryIntegrationYAMLFileParses(t *testing.T) {
	n := 0
	err := fs.WalkDir(os.DirFS("."), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || (filepath.Ext(p) != ".yaml" && filepath.Ext(p) != ".yml") {
			return err
		}
		n++
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var v any
		if err := yaml.Unmarshal(b, &v); err != nil {
			t.Errorf("%s: %v", p, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 10 {
		t.Fatalf("found only %d YAML files; is the test running in integrations/?", n)
	}
}

// The IRIS endpoint table keeps its path placeholders, which iris.py fills with str.format.
func TestIRISEndpointPathsKeepPlaceholders(t *testing.T) {
	b, err := os.ReadFile("dfir_iris/actions/endpoints.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var table map[string]map[string]struct {
		Method string `yaml:"method"`
		Path   string `yaml:"path"`
	}
	if err := yaml.Unmarshal(b, &table); err != nil {
		t.Fatal(err)
	}
	for gen, eps := range table {
		for name, ep := range eps {
			if ep.Method == "" || !strings.HasPrefix(ep.Path, "/") {
				t.Errorf("%s.%s: method %q path %q", gen, name, ep.Method, ep.Path)
			}
		}
	}
	if p := table["legacy_2_4"]["case_update"].Path; p != "/manage/cases/update/{case_id}" {
		t.Errorf("case_update path %q", p)
	}
}
