package integrations

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/yourorg/soar/internal/actions"
	"github.com/yourorg/soar/internal/engine"
	"github.com/yourorg/soar/internal/store/memory"
)

// The example playbooks must compile against the real action registry (built-ins plus
// every integration package), so they never drift from the manifests.
func TestExampleWorkflowsCompile(t *testing.T) {
	reg, err := actions.Build(actions.Deps{Store: memory.NewStore("soar"), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, Packs, nil)
	if err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob("../examples/workflows/*.yaml")
	if len(files) < 2 {
		t.Fatalf("examples not found: %v", files)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var def map[string]any
		if err := yaml.Unmarshal(raw, &def); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if _, errs := engine.Compile(def, reg); len(errs) > 0 {
			t.Errorf("%s: %+v", filepath.Base(f), errs)
		}
	}
}
