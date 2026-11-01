package schema

import (
	"reflect"
	"strings"
	"testing"
)

var testOpts = Options{Prefix: "soar", StateReplicas: 0, TimeSeriesReplicas: 0,
	Retention: Retention{Default: "7d", Classes: map[string]string{"cases": "3d", "idempotency": "ttl", "seen": "ttl"}}}

// Every JSON field of every domain type must exist in the strict mapping,
// otherwise OpenSearch would reject real documents at runtime.
func TestDomainFieldsExistInMapping(t *testing.T) {
	for _, e := range Entities() {
		e := e
		t.Run(e.Name, func(t *testing.T) {
			checkType(t, e.Name, reflect.TypeOf(e.Model), e.Fields())
		})
	}
}

func jsonName(f reflect.StructField) (string, bool) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false
	}
	name := strings.Split(tag, ",")[0]
	if name == "" {
		name = f.Name
	}
	return name, true
}

func checkType(t *testing.T, path string, typ reflect.Type, props Props) {
	t.Helper()
	for typ.Kind() == reflect.Ptr || typ.Kind() == reflect.Slice {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Anonymous {
			checkType(t, path, f.Type, props)
			continue
		}
		name, ok := jsonName(f)
		if !ok {
			continue
		}
		raw, found := props[name]
		if !found {
			t.Errorf("%s.%s: JSON field %q missing from mapping", path, f.Name, name)
			continue
		}
		p, _ := raw.(Props)
		if inner, ok := p["properties"].(Props); ok {
			checkType(t, path+"."+name, f.Type, inner)
		}
	}
}

func TestBuildAll(t *testing.T) {
	plans, err := BuildAll(testOpts)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 22 {
		t.Fatalf("want 22 entities from spec section 5, got %d", len(plans))
	}
	seen := map[string]bool{}
	for _, p := range plans {
		if seen[p.Alias] {
			t.Errorf("duplicate alias %s", p.Alias)
		}
		seen[p.Alias] = true
		if p.Mappings["dynamic"] != "strict" {
			t.Errorf("%s: mapping is not strict", p.Alias)
		}
		if (p.Entity.Kind == TimeSeries) != (p.Policy != nil) {
			t.Errorf("%s: ISM policy must exist exactly for time-series indices", p.Alias)
		}
		if p.Checksum == "" {
			t.Errorf("%s: empty checksum", p.Alias)
		}
	}
}

func TestChecksumStableAndSensitive(t *testing.T) {
	a, _ := BuildAll(testOpts)
	b, _ := BuildAll(testOpts)
	for i := range a {
		if a[i].Checksum != b[i].Checksum {
			t.Fatalf("%s: checksum not deterministic", a[i].Alias)
		}
	}
	o := testOpts
	o.StateReplicas = 1
	c, _ := BuildAll(o)
	if a[0].Checksum == c[0].Checksum {
		t.Error("replica change must change the checksum")
	}
	o = testOpts
	o.Retention = Retention{Default: "365d", Classes: map[string]string{"alerts": "365d"}}
	d, _ := BuildAll(o)
	for i := range a {
		if a[i].Alias == "soar-alerts" && a[i].Checksum == d[i].Checksum {
			t.Error("retention change must change the alerts checksum")
		}
	}
}

func TestPrefixIsolation(t *testing.T) {
	o := testOpts
	o.Prefix = "soartest"
	plans, _ := BuildAll(o)
	for _, p := range plans {
		if !strings.HasPrefix(p.TemplateName, "soartest-") {
			t.Errorf("%s not prefixed", p.TemplateName)
		}
	}
}

func TestRetentionParse(t *testing.T) {
	if _, err := ParseRetention([]byte("default: 7d\nclasses:\n  audit: 365d\n  seen: ttl\n")); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"default: 7\n", "default: 7d\nclasses:\n  x: forever\n", "classes: {}\n"} {
		if _, err := ParseRetention([]byte(bad)); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestShippedRetentionFile(t *testing.T) {
	r, err := LoadRetention("../../../../config/retention.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if r.For("cases") != "3d" || r.For("alerts") != "7d" || r.For("unknown") != "7d" {
		t.Errorf("unexpected values: %+v", r)
	}
	if _, err := BuildAll(Options{Prefix: "soar", Retention: r}); err != nil {
		t.Fatal(err)
	}
}
