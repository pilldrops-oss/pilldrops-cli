package mcp

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
)

// wantsToolCall reports whether the message (single or batch) invokes a tool —
// the only method class that requires credentials.
func wantsToolCall(body []byte) bool {
	var one struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(body, &one) == nil && one.Method != "" {
		return one.Method == "tools/call"
	}
	var many []struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(body, &many) == nil {
		for _, m := range many {
			if m.Method == "tools/call" {
				return true
			}
		}
	}
	return false
}

// publicOrigin mirrors the proxy-aware origin reconstruction the OAuth
// endpoints use (X-Forwarded-Proto is set behind Railway/cloudflared).
func publicOrigin(r *http.Request) string {
	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" {
		if r.TLS != nil {
			proto = "https"
		} else {
			proto = "http"
		}
	}
	return proto + "://" + r.Host
}

// HTTPServer serves MCP over the Streamable HTTP transport, stateless: every
// POST carries one JSON-RPC message (or batch) and gets one JSON response.
// Tool calls dispatch in-process to the same router that serves /v1 — no
// self-dial, and the caller's Authorization header flows through unchanged, so
// the /v1 layer keeps enforcing auth, org scoping, and limits.
type HTTPServer struct {
	API http.Handler // the app's main router (must route /v1/*)
}

func (s *HTTPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// Stateless server: no server-initiated SSE stream (GET) and no
		// session to delete. 405 tells spec-following clients exactly that.
		w.Header().Set("Allow", "POST")
		http.Error(w, "MCP endpoint accepts POST only (stateless streamable HTTP)", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	auth := r.Header.Get("Authorization")

	// Auth contract: discovery (initialize, tools/list, ping) is open so any
	// client can read the catalog; tools/call requires a bearer token. The 401
	// carries WWW-Authenticate pointing at the OAuth resource metadata, which
	// is what makes MCP clients start the browser sign-in flow automatically.
	if auth == "" && wantsToolCall(body) {
		base := publicOrigin(r)
		w.Header().Set("WWW-Authenticate",
			`Bearer resource_metadata="`+base+`/.well-known/oauth-protected-resource"`)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write(replyErr(nil, -32001,
			"authentication required: complete the OAuth flow your client just started, "+
				"or configure the header Authorization: Bearer <api-key> (dashboard → API keys)"))
		return
	}
	be := func(method, path string, reqBody []byte, ctype string) (int, []byte, error) {
		req := httptest.NewRequest(method, path, bytes.NewReader(reqBody))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		rec := httptest.NewRecorder()
		s.API.ServeHTTP(rec, req)
		return rec.Code, rec.Body.Bytes(), nil
	}
	resp := HandleMessage(body, be)
	if resp == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp)
}
