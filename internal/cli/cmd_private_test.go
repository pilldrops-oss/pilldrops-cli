package cli

import (
	"strings"
	"testing"
)

func TestPrivateTagGrantsListAndRevoke(t *testing.T) {
	f := newFakeConnectorServer(t, true)
	code, out, errS := authedCLI(t, f.srv, "private", "grant", "--tag", "backend", "prod/subnet:10.0.5.0/24")
	if code != ExitOK || !strings.Contains(out, "pg_9") || !strings.Contains(errS, "sb7") || !strings.Contains(errS, "10.128.0.0/16") {
		t.Fatalf("grant: %d %q %q", code, out, errS)
	}
	if got := f.tagBody.Load(); got != `{"connector":"prod","subnet":"10.0.5.0/24","tag":"backend"}` {
		t.Fatalf("body %q", got)
	}
	code, out, _ = authedCLI(t, f.srv, "private", "grants", "--tag", "backend")
	if code != ExitOK || !strings.Contains(out, "backend") || !strings.Contains(out, "10.0.5.0/24") || f.tagQuery.Load() != "tag=backend" {
		t.Fatalf("list: %d %q %v", code, out, f.tagQuery.Load())
	}
	if code, _, _ := authedCLI(t, f.srv, "private", "revoke", "pg_9"); code != ExitOK || f.revoked.Load() != "pg_9" {
		t.Fatalf("revoke: %d %v", code, f.revoked.Load())
	}
	for _, args := range [][]string{{"private"}, {"private", "grant", "prod/db"}, {"private", "grant", "--tag", "backend", "prod"}, {"private", "revoke"}} {
		if code, _, _ := authedCLI(t, f.srv, args...); code != ExitUsage {
			t.Errorf("%v: %d", args, code)
		}
	}
}
