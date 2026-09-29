package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// visibilityPortal records what the CLI asks for and answers like the node.
func visibilityPortal(t *testing.T, seen *map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(seen)
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":"sb_1","template":"base","status":"running","visibility":"private"}`))
	})
	mux.HandleFunc("POST /v1/sandboxes/sb_1/visibility", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(seen)
		if (*seen)["visibility"] == "org" {
			_, _ = w.Write([]byte(`{"id":"sb_1","visibility":"org","was":"private","wiped":["~/.claude/.credentials.json"]}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"sb_1","visibility":"private","was":"org","relogin":true}`))
	})
	mux.HandleFunc("POST /v1/sandboxes/sb_1/fork", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(seen)
		vis := "private"
		if (*seen)["visibility"] == "org" {
			vis = "org"
		}
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":"sb_2","status":"running","visibility":"` + vis + `"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestShareUnshareAndPrivateFlags(t *testing.T) {
	var seen map[string]any
	srv := visibilityPortal(t, &seen)

	// create --private asks for private; without it the request says nothing,
	// so the platform default (org) applies rather than the CLI imposing one.
	if code, _, errS := authedCLI(t, srv, "sandbox", "create", "--template", "base", "--private"); code != ExitOK {
		t.Fatalf("create --private = %d stderr=%s", code, errS)
	}
	if seen["visibility"] != "private" {
		t.Errorf("create --private sent visibility=%v, want private", seen["visibility"])
	}
	seen = nil
	if code, _, _ := authedCLI(t, srv, "sandbox", "create", "--template", "base"); code != ExitOK {
		t.Fatal("plain create failed")
	}
	if _, named := seen["visibility"]; named {
		t.Errorf("a plain create names a visibility (%v); it must leave the default alone", seen["visibility"])
	}

	code, out, errS := authedCLI(t, srv, "sandbox", "share", "sb_1")
	if code != ExitOK {
		t.Fatalf("share = %d stderr=%s", code, errS)
	}
	if seen["visibility"] != "org" {
		t.Errorf("share sent visibility=%v, want org", seen["visibility"])
	}
	if !strings.Contains(out, "wiped ~/.claude/.credentials.json") || !strings.Contains(out, "log in again") {
		t.Errorf("share output does not say what was wiped and that it is gone for good:\n%s", out)
	}

	if code, out, _ = authedCLI(t, srv, "sandbox", "unshare", "sb_1"); code != ExitOK {
		t.Fatalf("unshare = %d", code)
	}
	if seen["visibility"] != "private" {
		t.Errorf("unshare sent visibility=%v, want private", seen["visibility"])
	}
	if !strings.Contains(out, "not coming back") {
		t.Errorf("unshare output does not warn that the wiped logins are not restored:\n%s", out)
	}
}

func TestForkDefaultsToPrivateAndTakesShared(t *testing.T) {
	var seen map[string]any
	srv := visibilityPortal(t, &seen)

	code, out, errS := authedCLI(t, srv, "sandbox", "fork", "sb_1")
	if code != ExitOK {
		t.Fatalf("fork = %d stderr=%s", code, errS)
	}
	if _, named := seen["visibility"]; named {
		t.Errorf("a plain fork names a visibility (%v); the node's default is private", seen["visibility"])
	}
	if !strings.Contains(out, "private") {
		t.Errorf("fork output = %q, want the child's visibility", out)
	}

	if code, out, _ = authedCLI(t, srv, "sandbox", "fork", "--shared", "sb_1"); code != ExitOK {
		t.Fatalf("fork --shared = %d", code)
	}
	if seen["visibility"] != "org" {
		t.Errorf("fork --shared sent visibility=%v, want org", seen["visibility"])
	}
	if !strings.Contains(out, "org") {
		t.Errorf("fork --shared output = %q", out)
	}
}
