package security

// MaxJSONDepth caps the nesting of every JSON body the API and ingest accept (spec
// section 13: size, depth and CEL-cost limits). Real payloads stay far below it (a
// Wazuh alert nests about 5 levels, a workflow definition about 10); deeper input
// only serves to exhaust the parser or the CEL evaluator.
const MaxJSONDepth = 64

// JSONDepth returns the deepest nesting of objects and arrays in b, without decoding
// it. Brackets inside strings do not count. Malformed input is left to the decoder.
func JSONDepth(b []byte) int {
	depth, deepest := 0, 0
	inString, escaped := false, false
	for _, c := range b {
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > deepest {
				deepest = depth
			}
		case '}', ']':
			if depth > 0 {
				depth--
			}
		}
	}
	return deepest
}
