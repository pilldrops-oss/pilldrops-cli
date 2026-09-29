package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The platform explains every refusal with its own code. These are the exact
// bodies and statuses its handlers write:
//
//	egress_not_enforced        orchestrator/sandboxes.go:49  (501)
//	slug_taken                 httpapi/sandboxes.go:224      (409)
//	confirm_required           httpapi/backuproutes.go:38    (400)
//	backups_need_object_store  httpapi/backuproutes.go:28    (501)
//
// Before APIError carried a Code, all four were parsed and discarded and the
// "code" key held this process's exit status instead.
func TestPlatformErrorCodeReachesJSONOutput(t *testing.T) {
	cases := []struct {
		code   string
		status int
		msg    string
		args   []string
	}{
		{"egress_not_enforced", 501, "domain egress rules need the name layer", []string{"--json", "sandbox", "create"}},
		{"slug_taken", 409, `slug "demo" is taken by another sandbox`, []string{"--json", "sandbox", "create", "--slug", "demo"}},
		{"confirm_required", 400, "restoring overwrites the disk", []string{"--json", "backup", "restore", "sb_x", "bkp_1", "--confirm"}},
		{"backups_need_object_store", 501, "no object store is configured", []string{"--json", "backup", "create", "sb_x"}},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": tc.msg, "code": tc.code})
			}))
			defer srv.Close()

			t.Setenv("PILLDROPS_API_KEY", "sf_test")
			exit, _, errS := runCLI2(t, srv.URL, tc.args...)

			var v map[string]any
			if err := json.Unmarshal([]byte(errS), &v); err != nil {
				t.Fatalf("stderr is not one JSON value: %v\n%s", err, errS)
			}
			if v["code"] != tc.code {
				t.Errorf(`"code" = %v, want the platform's %q verbatim`, v["code"], tc.code)
			}
			if got := v["status"]; got != float64(tc.status) {
				t.Errorf(`"status" = %v, want %d`, got, tc.status)
			}
			if got := v["exit_code"]; got != float64(exit) {
				t.Errorf(`"exit_code" = %v, want the process's %d`, got, exit)
			}
			if !strings.Contains(str(v["error"]), tc.msg) {
				t.Errorf(`"error" = %v, want the platform's message`, v["error"])
			}
		})
	}
}

// A transport failure must stay distinguishable from all four: nothing was
// refused, so there is no platform code to carry.
func TestTransportFailureCarriesNoPlatformCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead := srv.URL
	srv.Close() // nothing is listening now

	t.Setenv("PILLDROPS_API_KEY", "sf_test")
	exit, _, errS := runCLI2(t, dead, "--json", "sandbox", "ls")

	var v map[string]any
	if err := json.Unmarshal([]byte(errS), &v); err != nil {
		t.Fatalf("stderr is not one JSON value: %v\n%s", err, errS)
	}
	if _, ok := v["code"]; ok {
		t.Errorf(`transport failure carries "code" = %v; it must be absent`, v["code"])
	}
	if _, ok := v["status"]; ok {
		t.Errorf(`transport failure carries "status" = %v; nothing answered`, v["status"])
	}
	if v["exit_code"] != float64(exit) || exit != ExitAPI {
		t.Errorf("exit_code = %v, exit = %d, want %d on both", v["exit_code"], exit, ExitAPI)
	}
}
