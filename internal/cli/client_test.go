package cli

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestClientJSONAuthAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/ok":
			if r.Header.Get("Authorization") != "Bearer sfc_k" {
				w.WriteHeader(401)
				w.Write([]byte(`{"error":"missing bearer token"}`))
				return
			}
			w.Write([]byte(`{"hello":"world"}`))
		case "/v1/denied":
			w.WriteHeader(401)
			w.Write([]byte(`{"error":"invalid api key"}`))
		case "/v1/teapot":
			w.WriteHeader(418)
			w.Write([]byte(`{"error":"short and stout"}`))
		}
	}))
	defer srv.Close()

	c := &Client{Base: srv.URL, Key: "sfc_k"}
	var out map[string]string
	if err := c.JSON("GET", "/v1/ok", nil, &out); err != nil || out["hello"] != "world" {
		t.Fatalf("ok call: out=%v err=%v", out, err)
	}

	err := c.JSON("GET", "/v1/denied", nil, nil)
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.Status != 401 || apiErr.Msg != "invalid api key" {
		t.Fatalf("denied err = %#v", err)
	}
	err = c.JSON("GET", "/v1/teapot", nil, nil)
	if apiErr, ok = err.(*APIError); !ok || apiErr.Status != 418 || apiErr.Msg != "short and stout" {
		t.Fatalf("teapot err = %#v", err)
	}
}

func TestClientRetriesOnceOn5xxGET(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
			w.Write([]byte(`{"error":"warming up"}`))
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := &Client{Base: srv.URL, Key: "k"}
	if err := c.JSON("GET", "/v1/x", nil, nil); err != nil {
		t.Fatalf("should have retried to success: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}

	// POST must NOT retry (not idempotent).
	calls.Store(0)
	if err := c.JSON("POST", "/v1/x", map[string]string{}, nil); err == nil {
		t.Fatal("first POST returned 503 — expected error, no retry")
	}
	if calls.Load() != 1 {
		t.Fatalf("POST calls = %d, want 1", calls.Load())
	}
}
