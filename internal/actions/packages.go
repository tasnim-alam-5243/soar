package actions

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Package is an integration package manifest: integrations/<name>/manifest.yaml
// (spec section 10). One package can provide actions, inbound sources and mappings.
type Package struct {
	Name             string         `yaml:"name" json:"name"`
	Title            string         `yaml:"title" json:"title"`
	Description      string         `yaml:"description" json:"description"`
	Version          string         `yaml:"version" json:"version"`
	Auth             map[string]any `yaml:"auth" json:"auth"`
	ConnectionSchema map[string]any `yaml:"connection_schema" json:"connection_schema"`
	Secrets          []string       `yaml:"secrets" json:"secrets"`
	Defaults         Manifest       `yaml:"defaults" json:"defaults"` // inherited by every action
	Actions          []Manifest     `yaml:"actions" json:"actions"`
}

// LoadPackages reads every */manifest.yaml under root in fsys.
func LoadPackages(fsys fs.FS, root string) ([]Package, error) {
	pattern := path.Join(root, "*", "manifest.yaml")
	if root == "" || root == "." {
		pattern = "*/manifest.yaml"
	}
	files, err := fs.Glob(fsys, pattern)
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []Package
	for _, f := range files {
		data, err := fs.ReadFile(fsys, f)
		if err != nil {
			return nil, err
		}
		var raw map[string]any
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		// normalise through JSON so nested schemas are plain map[string]any with float64 numbers
		b, err := json.Marshal(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		var p Package
		if err := json.Unmarshal(b, &p); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if p.Name == "" {
			p.Name = path.Base(path.Dir(f))
		}
		for i := range p.Actions {
			a := &p.Actions[i]
			inherit(a, p.Defaults)
			if a.Class == "" {
				a.Class = ClassPython
			}
			if a.Version == 0 {
				a.Version = 1
			}
			a.Connector = p.Name
			if a.Class == ClassPython {
				a.NeedsConnection = true
			}
			if err := a.Validate(); err != nil {
				return nil, fmt.Errorf("%s: %w", f, err)
			}
			if !strings.HasPrefix(a.ID, p.Name+".") && !strings.Contains(a.ID, ".") {
				return nil, fmt.Errorf("%s: action id %q must be namespaced (for example %s.%s)", f, a.ID, p.Name, a.ID)
			}
		}
		out = append(out, p)
	}
	return out, nil
}

func inherit(a *Manifest, d Manifest) {
	if len(a.RetryableErrors) == 0 {
		a.RetryableErrors = d.RetryableErrors
	}
	if a.Limits.TimeoutS == 0 {
		a.Limits.TimeoutS = d.Limits.TimeoutS
	}
	if a.Limits.MemoryMB == 0 {
		a.Limits.MemoryMB = d.Limits.MemoryMB
	}
	if a.Limits.CPU == 0 {
		a.Limits.CPU = d.Limits.CPU
	}
	if len(a.Limits.Egress) == 0 {
		a.Limits.Egress = d.Limits.Egress
	}
	if len(a.Secrets) == 0 {
		a.Secrets = d.Secrets
	}
	if a.Class == "" {
		a.Class = d.Class
	}
}

// RegisterPackages adds every package's actions (run by exec) and connector info.
func RegisterPackages(r *Registry, pkgs []Package, exec Executor) error {
	for _, p := range pkgs {
		info := ConnectorInfo{
			ID: p.Name, Title: p.Title, Description: p.Description, Version: p.Version,
			ConnectionSchema: p.ConnectionSchema, Secrets: p.Secrets,
		}
		if t, ok := p.Auth["type"].(string); ok {
			info.AuthType = t
		}
		for _, a := range p.Actions {
			if err := r.RegisterRemote(a, exec); err != nil {
				return err
			}
			info.Actions = append(info.Actions, a.ID)
		}
		sort.Strings(info.Actions)
		r.AddConnector(info)
	}
	return nil
}

// Build returns a registry with the built-in actions and the integration packages
// found in packs (nil for none). exec runs python actions and may be nil in roles
// that only need manifests (API validation, orchestrator routing).
func Build(d Deps, packs fs.FS, exec Executor) (*Registry, error) {
	r := NewRegistry()
	if err := RegisterBuiltins(r, d); err != nil {
		return nil, err
	}
	if packs != nil {
		pkgs, err := LoadPackages(packs, ".")
		if err != nil {
			return nil, err
		}
		if err := RegisterPackages(r, pkgs, exec); err != nil {
			return nil, err
		}
	}
	return r, nil
}
