// Package schema defines every OpenSearch index: mappings, settings, retention
// policies. It is the single source of truth that `soar migrate` applies.
package schema

// Props is a set of OpenSearch mapping properties.
type Props map[string]any

func kw() Props      { return Props{"type": "keyword"} }
func text() Props    { return Props{"type": "text"} }
func date() Props    { return Props{"type": "date"} }
func boolean() Props { return Props{"type": "boolean"} }
func integer() Props { return Props{"type": "integer"} }
func long() Props    { return Props{"type": "long"} }
func double() Props  { return Props{"type": "double"} }
func ipAddr() Props  { return Props{"type": "ip"} }
func binary() Props  { return Props{"type": "binary"} }

// textKW is searchable text plus an exact-match keyword sub-field.
func textKW() Props {
	return Props{"type": "text", "fields": Props{"raw": Props{"type": "keyword", "ignore_above": 512}}}
}

// stored is a string kept in _source but never indexed (long CEL, URLs, cursors).
func stored() Props { return Props{"type": "keyword", "index": false, "doc_values": false} }

// blob is an arbitrary JSON object kept in _source and not indexed. Used for
// workflow definitions, execution context and similar documents that are always
// fetched by ID, never searched by inner field.
func blob() Props { return Props{"type": "object", "enabled": false} }

// flat is an arbitrary JSON object whose leaf values are searchable as keywords
// (OpenSearch flat_object, 2.7+). Used for vendor-specific "ext" fields.
func flat() Props { return Props{"type": "flat_object"} }

// obj is a strict inner object.
func obj(p Props) Props { return Props{"type": "object", "dynamic": "strict", "properties": p} }

// nested is a strict nested array of objects (needed when several conditions
// must match the same array element, for example outbox published=false AND age).
func nested(p Props) Props { return Props{"type": "nested", "dynamic": "strict", "properties": p} }

func merge(a, b Props) Props {
	out := Props{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// common are the fields every document carries (spec section 5).
func common() Props {
	return Props{"id": kw(), "created_at": date(), "updated_at": date(), "schema_version": integer()}
}
