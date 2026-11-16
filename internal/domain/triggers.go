package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Triggers can be written in two places: inside a workflow definition (the spec 6 DSL
// form, used by the example playbooks) or as the workflow document's "triggers" field
// (what the UI sends). The API stores them in the document only (see SplitTriggers);
// AllTriggers also reads definitions stored before that rule existed, so the processor
// and the scheduler never miss a trigger whichever form was used.

// SplitTriggers returns a copy of def without its "triggers" key, the triggers that key
// held, and whether the key was present at all. def itself is not modified. Unknown
// trigger fields are an error, so a typo ("condition" for "match") is not silently lost.
func SplitTriggers(def JSON) (rest JSON, triggers []Trigger, declared bool, err error) {
	raw, declared := def["triggers"]
	if !declared {
		return def, nil, false, nil
	}
	rest = make(JSON, len(def))
	for k, v := range def {
		if k != "triggers" {
			rest[k] = v
		}
	}
	if raw == nil {
		return rest, nil, true, nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, nil, true, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&triggers); err != nil {
		return nil, nil, true, fmt.Errorf("must be an array of {type, kind, match, cron}: %w", err)
	}
	return rest, triggers, true, nil
}

// MergeTriggers returns a followed by the triggers of b that a does not already contain.
func MergeTriggers(a, b []Trigger) []Trigger {
	if len(b) == 0 {
		return a
	}
	seen := make(map[Trigger]bool, len(a)+len(b))
	out := make([]Trigger, 0, len(a)+len(b))
	for _, list := range [][]Trigger{a, b} {
		for _, t := range list {
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	return out
}

// AllTriggers is every trigger of a workflow: the document's own and any still written
// inside its definition. Readers that act on triggers (the processor's trigger index,
// the cron scheduler) use this rather than the Triggers field.
func (w Workflow) AllTriggers() []Trigger {
	_, inDef, _, err := SplitTriggers(w.Definition)
	if err != nil {
		return w.Triggers
	}
	return MergeTriggers(w.Triggers, inDef)
}
