package builtin

import (
	"context"
	"testing"
)

func TestLsof_ParamsRequired(t *testing.T) {
	_, err := Lsof{}.Execute(context.Background(), []byte(`{}`))
	if err == nil {
		t.Error("expected error when both pid and path missing")
	}
}

func TestLsof_ParseOutput(t *testing.T) {
	out := "p1234\nccron\nuroot\nf3u\ntREG\nn/var/log/syslog\np1234\nccron\nuroot\nf4u\ntDIR\nn/\n0\n"
	res := lsofResult{}
	parseLsof(out, &res)
	if got := len(res.Entries); got != 2 {
		t.Errorf("len(Entries) = %d, want 2 (out=%q)", got, out)
	}
	if len(res.Entries) >= 1 {
		if res.Entries[0].PID != 1234 {
			t.Errorf("PID = %d, want 1234", res.Entries[0].PID)
		}
		if res.Entries[0].Name != "/var/log/syslog" {
			t.Errorf("Name = %q, want /var/log/syslog", res.Entries[0].Name)
		}
	}
}
