package parsers

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

func init() { Register("syslog", syslogParser{}) }

// syslogParser reads RFC 5424 and RFC 3164 (BSD) messages, one per line (Step 10). A
// line it cannot split still becomes a record ({message}), so nothing a device sends
// is lost to a format quirk; the mapping decides what is usable.
//
// Fields: priority, facility, severity, severity_name, timestamp, hostname, app_name,
// procid, msgid, structured_data ({sd-id: {param: value}}), message, format
// ("rfc5424", "rfc3164" or "unknown"). Absent values ("-" in RFC 5424) are omitted.
type syslogParser struct{}

var severityNames = []string{"emergency", "alert", "critical", "error", "warning", "notice", "informational", "debug"}

func (syslogParser) Parse(body []byte) ([]map[string]any, error) {
	var out []map[string]any
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimRight(line, "\r\x00")
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		out = append(out, ParseSyslogLine(string(line)))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty body")
	}
	return out, nil
}

// ParseSyslogLine parses one syslog message.
func ParseSyslogLine(s string) map[string]any {
	rec := map[string]any{}
	rest := s
	if strings.HasPrefix(rest, "<") {
		if end := strings.IndexByte(rest, '>'); end > 1 && end <= 4 {
			if pri, err := strconv.Atoi(rest[1:end]); err == nil && pri >= 0 && pri <= 191 {
				rec["priority"], rec["facility"], rec["severity"] = pri, pri/8, pri%8
				rec["severity_name"] = severityNames[pri%8]
				rest = rest[end+1:]
			}
		}
	}
	if _, ok := rec["priority"]; !ok {
		rec["format"], rec["message"] = "unknown", s
		return rec
	}
	if len(rest) >= 2 && rest[0] >= '1' && rest[0] <= '9' && rest[1] == ' ' {
		parse5424(rec, rest[2:])
		rec["version"] = int(rest[0] - '0')
		return rec
	}
	parse3164(rec, rest)
	return rec
}

// RFC 5424: TIMESTAMP SP HOSTNAME SP APP-NAME SP PROCID SP MSGID SP STRUCTURED-DATA [SP MSG]
func parse5424(rec map[string]any, s string) {
	rec["format"] = "rfc5424"
	fields := []string{"timestamp", "hostname", "app_name", "procid", "msgid"}
	for _, f := range fields {
		var tok string
		tok, s, _ = strings.Cut(s, " ")
		if tok != "-" && tok != "" {
			rec[f] = tok
		}
	}
	sd, msg := splitStructuredData(s)
	if len(sd) > 0 {
		rec["structured_data"] = sd
	}
	msg = strings.TrimPrefix(msg, "\xef\xbb\xbf") // the BOM that marks UTF-8 text
	if msg != "" {
		rec["message"] = msg
	}
}

// splitStructuredData reads "-" or one or more [id k="v" ...] elements.
func splitStructuredData(s string) (map[string]any, string) {
	if strings.HasPrefix(s, "-") {
		return nil, strings.TrimPrefix(strings.TrimPrefix(s, "-"), " ")
	}
	sd := map[string]any{}
	i := 0
	for i < len(s) && s[i] == '[' {
		j := i + 1
		for j < len(s) && s[j] != ' ' && s[j] != ']' {
			j++
		}
		id := s[i+1 : j]
		params := map[string]any{}
		for j < len(s) && s[j] == ' ' {
			j++
			k := j
			for k < len(s) && s[k] != '=' && s[k] != ']' {
				k++
			}
			if k+1 >= len(s) || s[k] != '=' || s[k+1] != '"' {
				break
			}
			name := s[j:k]
			var val strings.Builder
			k += 2
			for k < len(s) && s[k] != '"' {
				if s[k] == '\\' && k+1 < len(s) && strings.ContainsRune(`"\]`, rune(s[k+1])) {
					k++
				}
				val.WriteByte(s[k])
				k++
			}
			params[name] = val.String()
			j = k + 1
		}
		for j < len(s) && s[j] != ']' {
			j++
		}
		sd[id] = params
		i = j + 1
	}
	return sd, strings.TrimPrefix(s[min(i, len(s)):], " ")
}

// RFC 3164: "Mmm dd hh:mm:ss HOSTNAME TAG[PID]: MSG" (devices vary; parts are optional).
func parse3164(rec map[string]any, s string) {
	rec["format"] = "rfc3164"
	if len(s) >= 15 && isMonth(s[:3]) && s[3] == ' ' && s[6] == ' ' && s[9] == ':' && s[12] == ':' {
		rec["timestamp"] = s[:15]
		s = strings.TrimPrefix(s[15:], " ")
		if host, rest, ok := strings.Cut(s, " "); ok && host != "" && !strings.HasSuffix(host, ":") {
			rec["hostname"], s = host, rest
		}
	}
	if tag, msg, ok := strings.Cut(s, ": "); ok && tag != "" && !strings.ContainsAny(tag, " ") {
		if name, pid, ok := strings.Cut(strings.TrimSuffix(tag, "]"), "["); ok {
			rec["app_name"], rec["procid"] = name, pid
		} else {
			rec["app_name"] = tag
		}
		s = msg
	}
	if s != "" {
		rec["message"] = s
	}
}

func isMonth(m string) bool {
	switch m {
	case "Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec":
		return true
	}
	return false
}
