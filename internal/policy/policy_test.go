package policy

import "testing"

func TestDefaults(t *testing.T) {
	e, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		r    Request
		want string
	}{
		{"read", Request{ActorType: "workflow", SideEffect: "read"}, Allow},
		{"write", Request{ActorType: "workflow", SideEffect: "write"}, Allow},
		{"destructive", Request{ActorType: "workflow", SideEffect: "destructive"}, RequireApproval},
		{"destructive pre-approved", Request{ActorType: "workflow", SideEffect: "destructive", PreApproved: true}, Allow},
		{"destructive dry run", Request{ActorType: "workflow", SideEffect: "destructive", DryRun: true}, Allow},
		{"agent write", Request{ActorType: "agent", SideEffect: "write"}, RequireApproval},
		{"agent read", Request{ActorType: "agent", SideEffect: "read"}, Allow},
		{"send under threshold", Request{
			SideEffect: "write", ApprovalPolicy: map[string]any{"recipients_over": float64(50)},
			Args: map[string]any{"to": []any{"a", "b"}, "bcc": []any{"c"}},
		}, Allow},
		{"send over threshold", Request{
			SideEffect: "write", ApprovalPolicy: map[string]any{"recipients_over": float64(2)},
			Args: map[string]any{"to": []any{"a", "b"}, "bcc": []any{"c"}},
		}, RequireApproval},
	}
	for _, c := range cases {
		if got := e.Decide(c.r); got.Effect != c.want {
			t.Errorf("%s: got %s (%s), want %s", c.name, got.Effect, got.Reason, c.want)
		}
	}
}

func TestRules(t *testing.T) {
	e, err := New([]Rule{
		{Name: "no-purge", When: `action == "outlook.mail.purge" && risk == "high"`, Effect: Deny},
		{Name: "trusted-delete", When: `action == "outlook.mail.delete" && size(args.mailboxes) <= 3`, Effect: Allow},
	})
	if err != nil {
		t.Fatal(err)
	}
	if d := e.Decide(Request{Action: "outlook.mail.purge", Risk: "high", SideEffect: "destructive"}); d.Effect != Deny || d.Rule != "no-purge" {
		t.Fatalf("%+v", d)
	}
	if d := e.Decide(Request{Action: "outlook.mail.delete", SideEffect: "destructive", Args: map[string]any{"mailboxes": []any{"a"}}}); d.Effect != Allow {
		t.Fatalf("%+v", d)
	}
	if d := e.Decide(Request{Action: "outlook.mail.delete", SideEffect: "destructive", Args: map[string]any{"mailboxes": []any{"a", "b", "c", "d"}}}); d.Effect != RequireApproval {
		t.Fatalf("falls through to the default: %+v", d)
	}
	if _, err := New([]Rule{{Name: "bad", When: "action ==", Effect: Allow}}); err == nil {
		t.Fatal("bad CEL accepted")
	}
	if _, err := New([]Rule{{Name: "bad", When: "true", Effect: "maybe"}}); err == nil {
		t.Fatal("bad effect accepted")
	}
}
