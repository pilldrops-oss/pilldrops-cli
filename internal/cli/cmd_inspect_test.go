package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func fakeInspectPortal(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes/sb_1/process/list", func(w http.ResponseWriter, r *http.Request) {
		// The node serves a bare array of {pid,cmd} (matches httpapi.handleProcList).
		w.Write([]byte(`[{"pid":1,"cmd":"python app.py"}]`))
	})
	mux.HandleFunc("GET /v1/sandboxes/sb_1/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"state":"running","vcpu":2,"mem_mib":1024,"uptime_seconds":12}`))
	})
	mux.HandleFunc("GET /v1/usage", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"total_starts":3,"vcpu_hours":0.5,"cost_usd":0.12}`))
	})
	return httptest.NewServer(mux)
}

func TestPsMetricsUsage(t *testing.T) {
	srv := fakeInspectPortal(t)
	defer srv.Close()
	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("SANDFORGE_API_URL", srv.URL)
	t.Setenv("SANDFORGE_API_KEY", "sfc_good")

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"ps", "sb_1"}, "python app.py"},
		{[]string{"metrics", "sb_1"}, "running"},
		{[]string{"usage"}, "vcpu_hours"},
	} {
		var out, errb bytes.Buffer
		if code := Run(tc.args, &out, &errb); code != ExitOK {
			t.Fatalf("%v = %d stderr=%s", tc.args, code, errb.String())
		}
		if !strings.Contains(out.String(), tc.want) {
			t.Fatalf("%v out = %q, want %q", tc.args, out.String(), tc.want)
		}
	}
}

func TestPsMissingIDUsage(t *testing.T) {
	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("SANDFORGE_API_KEY", "sfc_good")
	t.Setenv("SANDFORGE_API_URL", "http://unused.invalid")
	var out, errb bytes.Buffer
	if code := Run([]string{"ps"}, &out, &errb); code != ExitUsage {
		t.Fatalf("ps without id = %d, want %d", code, ExitUsage)
	}
}
