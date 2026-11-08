package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// featureServer answers /v1/capabilities with body and counts everything else.
func featureServer(t *testing.T, capabilities string) *int {
	t.Helper()
	other := new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/capabilities" {
			_, _ = w.Write([]byte(capabilities))
			return
		}
		*other++
		_, _ = w.Write([]byte(`{"jobs":[],"sandboxes":[]}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("PILLDROPS_API_URL", srv.URL)
	t.Setenv("PILLDROPS_API_KEY", "k")
	return other
}

func TestMissingFeatureIsRefusedBeforeAnyRequest(t *testing.T) {
	other := featureServer(t, `{"features":[]}`)
	for _, args := range [][]string{
		{"cron", "list"},
		{"chat-link", "sb_1"},
		{"sandbox", "integrations", "sb_1"},
		{"sandbox", "create", "--template", "base", "--volume", "v1:/data"},
	} {
		var out, errOut bytes.Buffer
		code := Run(append([]string{"--json"}, args...), &out, &errOut)
		if code != ExitAPI {
			t.Errorf("%v: exit %d, want %d: %s", args, code, ExitAPI, errOut.String())
		}
		var failure struct {
			Code string `json:"code"`
			Hint string `json:"hint"`
		}
		if err := json.Unmarshal(errOut.Bytes(), &failure); err != nil {
			t.Fatalf("%v: %v: %s", args, err, errOut.String())
		}
		if failure.Code != "feature_unavailable" || !strings.Contains(failure.Hint, "PillDrops Cloud") {
			t.Errorf("%v: %+v", args, failure)
		}
	}
	if *other != 0 {
		t.Errorf("%d requests went past the refusal", *other)
	}
}

func TestListedFeatureRuns(t *testing.T) {
	other := featureServer(t, `{"features":["cron"]}`)
	var out, errOut bytes.Buffer
	if code := Run([]string{"--json", "cron", "list"}, &out, &errOut); code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if *other != 1 {
		t.Errorf("%d requests reached the route, want 1", *other)
	}
}

// A server too old to report, or one that cannot be asked, keeps today's behaviour.
func TestUnknownFeaturesLeaveEverythingOn(t *testing.T) {
	for name, capabilities := range map[string]string{"old server": `{"scope":"fleet"}`, "unreadable": `not json`} {
		t.Run(name, func(t *testing.T) {
			other := featureServer(t, capabilities)
			var out, errOut bytes.Buffer
			if code := Run([]string{"--json", "cron", "list"}, &out, &errOut); code != ExitOK || *other != 1 {
				t.Fatalf("exit %d, %d route requests: %s", code, *other, errOut.String())
			}
			var help bytes.Buffer
			Run([]string{"help"}, &help, &errOut)
			if !strings.Contains(help.String(), "cron list") || strings.Contains(help.String(), "not on this server") {
				t.Errorf("help hid commands on a server that said nothing:\n%s", help.String())
			}
		})
	}
}

func TestHelpAndCatalogFollowTheServer(t *testing.T) {
	featureServer(t, `{"features":["volume"]}`)
	var help, errOut bytes.Buffer
	Run([]string{"help"}, &help, &errOut)
	for _, hidden := range []string{"cron list", "agent ask", "team login", "sandbox integrations"} {
		if strings.Contains(help.String(), hidden) {
			t.Errorf("help shows %q", hidden)
		}
	}
	for _, shown := range []string{"sandbox create", "--volume", "volume create", "not on this server: control-plane, team, agent, cron"} {
		if !strings.Contains(help.String(), shown) {
			t.Errorf("help is missing %q", shown)
		}
	}

	var out bytes.Buffer
	Run([]string{"--json", "ai"}, &out, &errOut)
	var catalog struct {
		Catalog     string   `json:"catalog"`
		Commands    []string `json:"commands"`
		Unsupported []string `json:"unsupported_features"`
	}
	if err := json.Unmarshal(out.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(catalog.Commands, "\n")
	if strings.Contains(joined, "cron") || strings.Contains(joined, "sandbox integrations") || !strings.Contains(joined, "volume create") {
		t.Errorf("commands: %s", joined)
	}
	if strings.Contains(catalog.Catalog, "## Teams") {
		t.Error("catalog explains team sign-in on a server without it")
	}
	if len(catalog.Unsupported) == 0 {
		t.Error("catalog does not say what is missing")
	}
}

func TestSingleNodeRefusesControlPlaneCommands(t *testing.T) {
	other := featureServer(t, `{"features":[]}`)
	for _, args := range [][]string{
		{"env", "list"}, {"domain", "list"}, {"snapshot", "list"}, {"project", "list"},
		{"sandbox", "expose", "sb_1", "8000"}, {"sandbox", "forwards", "sb_1"}, {"sandbox", "unexpose", "sb_1", "8000"},
		{"sandbox", "create", "--from-snapshot", "golden"},
	} {
		var out, errOut bytes.Buffer
		code := Run(append([]string{"--json"}, args...), &out, &errOut)
		if code != ExitAPI {
			t.Errorf("%v: exit %d, want %d: %s", args, code, ExitAPI, errOut.String())
		}
		if !strings.Contains(errOut.String(), "control plane") {
			t.Errorf("%v: %s", args, errOut.String())
		}
	}
	if *other != 0 {
		t.Errorf("%d requests went past the refusal", *other)
	}
}
