package schema

import (
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// Retention is the parsed config/retention.yaml (spec section 5.2).
type Retention struct {
	Default string            `yaml:"default"`
	Classes map[string]string `yaml:"classes"`
}

var durationRe = regexp.MustCompile(`^[0-9]+[dh]$`)

// TTL marks a class that is cleaned by its own expires_at field instead of age.
const TTL = "ttl"

// ParseRetention parses and validates retention YAML.
func ParseRetention(data []byte) (Retention, error) {
	var r Retention
	if err := yaml.Unmarshal(data, &r); err != nil {
		return r, fmt.Errorf("retention: parse yaml: %w", err)
	}
	if !durationRe.MatchString(r.Default) {
		return r, fmt.Errorf("retention: default %q must look like 7d or 36h", r.Default)
	}
	for class, v := range r.Classes {
		if v != TTL && !durationRe.MatchString(v) {
			return r, fmt.Errorf("retention: class %q has invalid value %q (use 7d, 36h or ttl)", class, v)
		}
	}
	return r, nil
}

// LoadRetention reads and validates a retention file.
func LoadRetention(path string) (Retention, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Retention{}, fmt.Errorf("retention: read %s: %w", path, err)
	}
	return ParseRetention(data)
}

// For returns the retention for a class, falling back to the default.
func (r Retention) For(class string) string {
	if v, ok := r.Classes[class]; ok {
		return v
	}
	return r.Default
}
