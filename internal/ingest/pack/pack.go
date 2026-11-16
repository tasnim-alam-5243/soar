// Package pack loads source packs (mapping, filter set, sources, lists as YAML) and
// installs them into the store. Install is idempotent and never overwrites edits.
package pack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/store"
)

// Bundle is everything one product pack provides.
type Bundle struct {
	Product  string
	Mappings []domain.Mapping
	Filters  []domain.FilterSet
	Sources  []domain.Source
	Lists    []domain.List
}

func readYAML(fsys fs.FS, dir string) (map[string]map[string]any, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]map[string]any{}, nil // a pack may omit filters or lists
	}
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]any{}
	for _, e := range entries {
		if e.IsDir() || path.Ext(e.Name()) != ".yaml" {
			continue
		}
		b, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var m map[string]any
		if err := yaml.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("%s/%s: %w", dir, e.Name(), err)
		}
		out[e.Name()] = m
	}
	return out, nil
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func intOf(m map[string]any, k string) int { n, _ := m[k].(int); return n }

func strs(v any) []string {
	out := []string{} // never nil: an empty list must be stored as [], not null
	list, _ := v.([]any)
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// jsonish converts YAML-decoded values (map[string]any with int leaves) to what JSON would give.
func jsonish(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

// Checksum is a stable hash of a definition.
func Checksum(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Load reads product/{mappings,filters,sources,lists}/*.yaml from fsys.
func Load(fsys fs.FS, product string) (*Bundle, error) {
	b := &Bundle{Product: product}

	maps, err := readYAML(fsys, path.Join(product, "mappings"))
	if err != nil {
		return nil, err
	}
	for file, m := range maps {
		name, ver := str(m, "id"), intOf(m, "version")
		if name == "" || ver < 1 {
			return nil, fmt.Errorf("mappings/%s: id and version are required", file)
		}
		def := jsonish(m)
		delete(def, "id")
		delete(def, "version")
		b.Mappings = append(b.Mappings, domain.Mapping{
			Meta: domain.Meta{ID: domain.DeterministicID("map", name, ver)}, Name: name, Version: ver,
			Kind: str(m, "kind"), Definition: def, Checksum: Checksum(def),
		})
	}

	fl, err := readYAML(fsys, path.Join(product, "filters"))
	if err != nil {
		return nil, err
	}
	for file, m := range fl {
		name, ver := str(m, "id"), intOf(m, "version")
		if name == "" || ver < 1 {
			return nil, fmt.Errorf("filters/%s: id and version are required", file)
		}
		j := jsonish(m)
		var rules []domain.JSON
		for _, r := range j["rules"].([]any) {
			rules = append(rules, r.(map[string]any))
		}
		b.Filters = append(b.Filters, domain.FilterSet{
			Meta: domain.Meta{ID: domain.DeterministicID("flt", name, ver)}, Name: name, Version: ver,
			Mode: str(m, "mode"), Default: str(m, "default"), Rules: rules, Checksum: Checksum(rules),
		})
	}

	srcs, err := readYAML(fsys, path.Join(product, "sources"))
	if err != nil {
		return nil, err
	}
	for file, m := range srcs {
		name := str(m, "name")
		if name == "" {
			return nil, fmt.Errorf("sources/%s: name is required", file)
		}
		j := jsonish(m)
		enabled, _ := m["enabled"].(bool)
		s := domain.Source{
			Meta: domain.Meta{ID: domain.DeterministicID(domain.PrefixSource, name)}, Name: name, System: str(m, "system"),
			Adapter: str(m, "adapter"), Parser: str(m, "parser"), MappingID: str(m, "mapping"), Schedule: str(m, "schedule"),
			Filters: strs(m["filters"]), Enabled: enabled,
		}
		s.Auth, _ = j["auth"].(map[string]any)
		s.Dedup, _ = j["dedup"].(map[string]any)
		s.RateLimit, _ = j["rate_limit"].(map[string]any)
		s.Config, _ = j["config"].(map[string]any)
		b.Sources = append(b.Sources, s)
	}

	lists, err := readYAML(fsys, path.Join(product, "lists"))
	if err != nil {
		return nil, err
	}
	for file, m := range lists {
		name := str(m, "name")
		if name == "" {
			return nil, fmt.Errorf("lists/%s: name is required", file)
		}
		b.Lists = append(b.Lists, domain.List{Meta: domain.Meta{ID: domain.DeterministicID("lst", name)}, Name: name, Items: strs(m["items"])})
	}

	sort.Slice(b.Mappings, func(i, j int) bool { return b.Mappings[i].Name < b.Mappings[j].Name })
	sort.Slice(b.Sources, func(i, j int) bool { return b.Sources[i].Name < b.Sources[j].Name })
	return b, nil
}

// Report says what Install did, one line per object.
type Report struct{ Lines []string }

func (r *Report) add(format string, a ...any) { r.Lines = append(r.Lines, fmt.Sprintf(format, a...)) }

// Install creates what is missing. Mapping and filter versions are immutable: if the
// stored version differs from the pack's, Install reports it and changes nothing
// (publish a new version instead). Sources and lists are created once and then belong
// to the operator, so edits made through the API survive upgrades.
func Install(ctx context.Context, st *store.Store, b *Bundle) (Report, error) {
	var rep Report
	for _, m := range b.Mappings {
		_, err := st.Mappings.Create(ctx, m)
		switch {
		case err == nil:
			rep.add("mapping %s@%d: created", m.Name, m.Version)
		case errors.Is(err, store.ErrAlreadyExists):
			cur, _, gerr := st.Mappings.Get(ctx, m.ID)
			if gerr != nil {
				return rep, gerr
			}
			if cur.Checksum != m.Checksum {
				rep.add("mapping %s@%d: differs from the pack; versions are immutable, bump the pack version to change it", m.Name, m.Version)
			} else {
				rep.add("mapping %s@%d: unchanged", m.Name, m.Version)
			}
		default:
			return rep, err
		}
	}
	for _, f := range b.Filters {
		_, err := st.Filters.Create(ctx, f)
		switch {
		case err == nil:
			rep.add("filter set %s@%d: created", f.Name, f.Version)
		case errors.Is(err, store.ErrAlreadyExists):
			cur, _, gerr := st.Filters.Get(ctx, f.ID)
			if gerr != nil {
				return rep, gerr
			}
			if cur.Checksum != f.Checksum {
				rep.add("filter set %s@%d: differs from the pack; versions are immutable, bump the pack version to change it", f.Name, f.Version)
			} else {
				rep.add("filter set %s@%d: unchanged", f.Name, f.Version)
			}
		default:
			return rep, err
		}
	}
	for _, l := range b.Lists {
		if _, err := st.Lists.Create(ctx, l); err == nil {
			rep.add("list %s: created", l.Name)
		} else if errors.Is(err, store.ErrAlreadyExists) {
			rep.add("list %s: kept (operator-owned)", l.Name)
		} else {
			return rep, err
		}
	}
	for _, s := range b.Sources {
		if _, err := st.Sources.Create(ctx, s); err == nil {
			rep.add("source %s: created (enabled=%v)", s.Name, s.Enabled)
		} else if errors.Is(err, store.ErrAlreadyExists) {
			rep.add("source %s: kept (operator-owned)", s.Name)
		} else {
			return rep, err
		}
	}
	return rep, nil
}
