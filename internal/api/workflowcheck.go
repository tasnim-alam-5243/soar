package api

import (
	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/engine"
)

// validateDefinition runs the engine's full save-time validation (spec section 6):
// structure, references, cycles, reachability, CEL compilation, action existence and
// parameters against the action manifests, and limits.
func (s *Server) validateDefinition(def map[string]any) []FieldError {
	_, errs := engine.Compile(def, s.catalog())
	return fieldErrors(errs)
}

func triggerErrors(ts []any) []FieldError { return fieldErrors(engine.TriggerErrors(ts)) }

// liftTriggers moves triggers written inside a definition (the spec 6 DSL form, as in
// examples/workflows) into the workflow document, where the processor and the cron
// scheduler read them, merged with the document-level triggers. The stored definition
// no longer carries them, so there is one place to edit them; the UI does the same.
// declared reports whether the definition had a "triggers" key at all.
func liftTriggers(def map[string]any, top []domain.Trigger) (rest map[string]any, triggers []domain.Trigger, declared bool, err error) {
	raw, declared := def["triggers"]
	if !declared {
		return def, top, false, nil
	}
	if raw != nil {
		if errs := fieldErrors(engine.DefinitionTriggerErrors(raw)); len(errs) > 0 {
			return nil, nil, true, unprocessable(errs)
		}
	}
	rest, inDef, _, err := domain.SplitTriggers(def)
	if err != nil {
		return nil, nil, true, unprocessable([]FieldError{{Path: "definition.triggers", Message: err.Error()}})
	}
	return rest, domain.MergeTriggers(top, inDef), true, nil
}
