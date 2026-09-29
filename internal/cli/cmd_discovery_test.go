package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// GET /v1/capabilities is answered by both tiers: the control plane returns the
// fleet aggregate, a bare node its own. The point of the verb is that a refusal
// becomes predictable — a Docker fleet reports tag_networks false, and a create
// naming tags on it is a 501 that could have been foreseen.
func TestCapabilitiesReportsWhatTheDeploymentCannotDo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/capabilities" {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write([]byte(`{"scope":"fleet","runtimes":["docker"],"tag_networks":false,
			"egress_domains":false,"checkpoint":false,"standby":false,"custom_domains":true,
			"nodes":{"total":1,"online":1}}`))
	}))
	defer srv.Close()
	t.Setenv("PILLDROPS_API_KEY", "sf_test")

	code, out, errS := runCLI2(t, srv.URL, "--json", "capabilities")
	if code != ExitOK {
		t.Fatalf("capabilities = %d: %s", code, errS)
	}
	var caps map[string]any
	if err := json.Unmarshal([]byte(out), &caps); err != nil {
		t.Fatalf("capabilities --json: %v\n%s", err, out)
	}
	if caps["tag_networks"] != false || caps["scope"] != "fleet" {
		t.Fatalf("capabilities = %v, want the fleet answer with tag_networks false", caps)
	}

	// The human rendering must show it too, false and all: a capability that
	// is missing from the table reads as one that is present.
	code, out, _ = runCLI2(t, srv.URL, "capabilities")
	if code != ExitOK || !strings.Contains(out, "tag_networks") || !strings.Contains(out, "false") {
		t.Fatalf("capabilities table = %q", out)
	}
	// And the nested object is not dropped on the floor.
	if !strings.Contains(out, "online=1") {
		t.Fatalf("capabilities table lost the node counts: %q", out)
	}
}

func TestLimitsReportsThePlanCeilings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/limits" {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write([]byte(`{"plan":{"name":"free","max_sandboxes":2},"hours":{"used":0.5},
			"storage":{"used_mb":10,"cap_mb":2048,"sandboxes":1,"measured":true}}`))
	}))
	defer srv.Close()
	t.Setenv("PILLDROPS_API_KEY", "sf_test")

	code, out, errS := runCLI2(t, srv.URL, "--json", "limits")
	if code != ExitOK {
		t.Fatalf("limits = %d: %s", code, errS)
	}
	var lim map[string]any
	if err := json.Unmarshal([]byte(out), &lim); err != nil {
		t.Fatalf("limits --json: %v\n%s", err, out)
	}
	plan, _ := lim["plan"].(map[string]any)
	if plan["max_sandboxes"] != float64(2) {
		t.Fatalf("limits = %v, want the plan's ceilings", lim)
	}
}
