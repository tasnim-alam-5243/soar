package parsers

import (
	"reflect"
	"testing"
)

func TestSyslogParser(t *testing.T) {
	p, ok := Get("syslog")
	if !ok {
		t.Fatal("syslog parser not registered")
	}
	recs, err := p.Parse([]byte(
		`<165>1 2026-11-11T18:00:00.003Z fw01 sshd 4321 ID47 [exampleSDID@32473 iut="3" eventSource="App\"lication"][meta seq="1"] ` + "\ufeff" + `Failed password for root` + "\r\n" +
			`<34>Nov 11 18:00:01 web01 su[812]: 'su root' failed for alice on /dev/pts/8` + "\n" +
			`<13>1 - - - - - - plain` + "\n\n" +
			`no priority at all`))
	if err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{
		{
			"priority": 165, "facility": 20, "severity": 5, "severity_name": "notice", "format": "rfc5424", "version": 1,
			"timestamp": "2026-11-11T18:00:00.003Z", "hostname": "fw01", "app_name": "sshd", "procid": "4321", "msgid": "ID47",
			"structured_data": map[string]any{"exampleSDID@32473": map[string]any{"iut": "3", "eventSource": `App"lication`}, "meta": map[string]any{"seq": "1"}},
			"message":         "Failed password for root",
		},
		{
			"priority": 34, "facility": 4, "severity": 2, "severity_name": "critical", "format": "rfc3164",
			"timestamp": "Nov 11 18:00:01", "hostname": "web01", "app_name": "su", "procid": "812",
			"message": "'su root' failed for alice on /dev/pts/8",
		},
		{"priority": 13, "facility": 1, "severity": 5, "severity_name": "notice", "format": "rfc5424", "version": 1, "message": "plain"},
		{"format": "unknown", "message": "no priority at all"},
	}
	if len(recs) != len(want) {
		t.Fatalf("got %d records: %v", len(recs), recs)
	}
	for i := range want {
		if !reflect.DeepEqual(recs[i], want[i]) {
			t.Errorf("record %d:\n got  %v\n want %v", i, recs[i], want[i])
		}
	}
	if _, err := p.Parse([]byte("\n \n")); err == nil {
		t.Error("an empty body must be an error")
	}
}
