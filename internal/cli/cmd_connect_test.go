package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeTerminal stands in for a sandbox: GET /v1/sandboxes/sb_1 hands out a
// terminal_url exactly as the platform does (a preview link for port 7681 with
// its access token on the query), and /preview/sb_1/7681/ws is a real WebSocket
// speaking envd's protocol. shell runs with the upgraded connection.
func fakeTerminal(t *testing.T, shell func(*websocket.Conn)) *httptest.Server {
	t.Helper()
	up := websocket.Upgrader{}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("GET /v1/sandboxes/sb_1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"sb_1","terminal_url":"` + srv.URL + `/preview/sb_1/7681/?token=tok123"}`))
	})
	// A standalone node embeds ?ptok= and serves no mint route at all, so any
	// mint attempt on this path is the client ignoring a credential it was given.
	mux.HandleFunc("GET /v1/sandboxes/sb_1/preview-token", func(w http.ResponseWriter, _ *http.Request) {
		t.Error("minted a preview token for a link that already carried one")
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/preview/sb_1/7681/ws", func(w http.ResponseWriter, r *http.Request) {
		// The gate credential must survive the rewrite from preview link to
		// websocket URL — without it the real proxy answers 403, not a shell.
		if r.URL.Query().Get("token") != "tok123" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		shell(ws)
	})
	return srv
}

// The whole point of the verb: keystrokes reach the remote PTY and its output
// comes back. A client that only dialled would print nothing and fail here.
func TestConnectPumpsBytesBothWays(t *testing.T) {
	srv := fakeTerminal(t, func(ws *websocket.Conn) {
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.TextMessage {
				continue // a resize control frame; not input
			}
			if len(data) == 1 && data[0] == 0x04 { // EOT: the shell would exit
				_ = ws.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
					time.Now().Add(time.Second))
				return
			}
			if err := ws.WriteMessage(websocket.BinaryMessage, []byte("pty<"+string(data)+">")); err != nil {
				return
			}
		}
	})
	cpEnv(t, srv.URL)
	stdin = strings.NewReader("whoami\n")
	defer func() { stdin = os.Stdin }()

	code, out, errS := runCLI2(t, srv.URL, "connect", "sb_1")
	if code != ExitOK {
		t.Fatalf("connect = %d, want 0 (stderr %q)", code, errS)
	}
	if !strings.Contains(out, "pty<whoami\n>") {
		t.Fatalf("remote output never reached stdout: %q", out)
	}
}

// Input goes as BINARY frames on purpose. envd treats a TEXT frame that parses
// as {"type":"resize",...} as a control message, so sending input as text would
// silently swallow that line instead of typing it.
func TestConnectSendsInputAsBinarySoResizeJSONIsNotSwallowed(t *testing.T) {
	line := `{"type":"resize","cols":9,"rows":9}` + "\n"
	got := make(chan string, 4)
	srv := fakeTerminal(t, func(ws *websocket.Conn) {
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				close(got)
				return
			}
			if len(data) == 1 && data[0] == 0x04 {
				_ = ws.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
					time.Now().Add(time.Second))
				close(got)
				return
			}
			if mt == websocket.TextMessage {
				got <- "TEXT:" + string(data)
				continue
			}
			got <- "BINARY:" + string(data)
		}
	})
	cpEnv(t, srv.URL)
	stdin = strings.NewReader(line)
	defer func() { stdin = os.Stdin }()

	if code, _, errS := runCLI2(t, srv.URL, "connect", "sb_1"); code != ExitOK {
		t.Fatalf("connect = %d (stderr %q)", code, errS)
	}
	for f := range got {
		if strings.HasPrefix(f, "TEXT:") {
			t.Fatalf("input was sent as a text frame and envd would eat it: %q", f)
		}
		if f != "BINARY:"+line {
			t.Fatalf("frame = %q, want the line verbatim", f)
		}
	}
}

// A dropped connection must not look like a shell that exited. The server here
// vanishes without a close frame; the client must say so and exit non-zero.
func TestConnectTellsADropApartFromTheShellExiting(t *testing.T) {
	srv := fakeTerminal(t, func(ws *websocket.Conn) {
		// Rip the TCP connection out from under the session: no close frame.
		if c := ws.UnderlyingConn(); c != nil {
			_ = c.Close()
		}
	})
	cpEnv(t, srv.URL)
	stdin = strings.NewReader("")
	defer func() { stdin = os.Stdin }()

	code, _, errS := runCLI2(t, srv.URL, "connect", "sb_1")
	if code != ExitAPI {
		t.Fatalf("dropped connection = %d, want %d", code, ExitAPI)
	}
	if !strings.Contains(errS, "was lost") {
		t.Fatalf("a drop must be reported, got stderr %q", errS)
	}
}

// A refusal at the preview gate is a 403 with an HTML page; the exit code and
// the message have to survive it rather than surfacing "bad handshake".
func TestConnectReportsAGateRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/sandboxes/") {
			_, _ = w.Write([]byte(`{"terminal_url":"http://` + r.Host + `/preview/sb_1/7681/?token=t"}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<!doctype html><title>Private sandbox</title>"))
	}))
	defer srv.Close()
	cpEnv(t, srv.URL)

	code, _, errS := runCLI2(t, srv.URL, "connect", "sb_1")
	if code != ExitAPI {
		t.Fatalf("code = %d, want %d", code, ExitAPI)
	}
	if !strings.Contains(errS, "HTTP 403") || !strings.Contains(errS, "preview gate") {
		t.Fatalf("stderr = %q, want the gate refusal explained", errS)
	}
}

// --print is the scriptable half: it must build the same URL the session dials.
func TestConnectPrintBuildsTheWebsocketURL(t *testing.T) {
	srv := fakeTerminal(t, func(*websocket.Conn) {})
	cpEnv(t, srv.URL)
	code, out, errS := runCLI2(t, srv.URL, "--json", "connect", "sb_1", "--print")
	if code != ExitOK {
		t.Fatalf("connect --print = %d (%s)", code, errS)
	}
	var v struct{ URL string }
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("--print is not JSON: %v (%s)", err, out)
	}
	want := "ws://" + strings.TrimPrefix(srv.URL, "http://") + "/preview/sb_1/7681/ws?token=tok123"
	if v.URL != want {
		t.Fatalf("url = %q, want %q", v.URL, want)
	}
}

func TestWSTerminalURL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// Path form, as the control plane mints it (browser-hop ?token=).
		{"https://pilldrops.com/preview/sb_1/7681/?token=abc",
			"wss://pilldrops.com/preview/sb_1/7681/ws?token=abc"},
		// Host form, as a node with a sandbox domain mints it (node-hop ?ptok=).
		{"http://7681-sb_1.sbx.test:8080/?ptok=xyz",
			"ws://7681-sb_1.sbx.test:8080/ws?ptok=xyz"},
		// No token at all: a public preview.
		{"https://pilldrops.com/preview/sb_1/7681/", "wss://pilldrops.com/preview/sb_1/7681/ws"},
	} {
		got, err := wsTerminalURL(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("wsTerminalURL(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	if _, err := wsTerminalURL("ftp://x/y"); err == nil {
		t.Error("a non-http scheme must be refused, not dialled")
	}
}

// The control frame envd parses is a fixed JSON shape; a renamed field would
// resize nothing and fail silently, which is the worst kind of terminal bug.
func TestResizeFrameMatchesEnvdsWireShape(t *testing.T) {
	b, err := json.Marshal(resizeMsg{Type: "resize", Cols: 120, Rows: 40})
	if err != nil || string(b) != `{"type":"resize","cols":120,"rows":40}` {
		t.Fatalf("resize frame = %s, %v", b, err)
	}
}

// The fleet shape, which the standalone-node tests above cannot reach. A node
// joined to a control plane mints NO token into terminal_url (previewToken
// returns "" whenever it verifies Ed25519) and sanitizeNodeURLs relays the bare
// value untouched, so terminal_url names the NODE's own address with no
// credential. The client must mint one and rebuild the link against the tier it
// is actually talking to. Both halves are load-bearing: the gate here refuses
// without the token, and the node address is a dead port.
func TestConnectMintsATokenWhenTheLinkCarriesNone(t *testing.T) {
	var minted []string
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("GET /v1/sandboxes/sb_1", func(w http.ResponseWriter, _ *http.Request) {
		// Port 9 is discard: nothing serves it. A client that dialled the node
		// address the platform published cannot reach anything.
		_, _ = w.Write([]byte(`{"id":"sb_1","terminal_url":"http://127.0.0.1:9/preview/sb_1/7681/"}`))
	})
	mux.HandleFunc("GET /v1/sandboxes/sb_1/preview-token", func(w http.ResponseWriter, r *http.Request) {
		minted = append(minted, r.URL.Query().Get("port"))
		_, _ = w.Write([]byte(`{"token":"tok7681","port":7681}`))
	})
	up := websocket.Upgrader{}
	mux.HandleFunc("/preview/sb_1/7681/ws", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "tok7681" {
			http.Error(w, "invalid or missing preview token", http.StatusForbidden)
			return
		}
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		_ = ws.WriteMessage(websocket.BinaryMessage, []byte("fleet-pty<"+string(data)+">"))
		_ = ws.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	})
	cpEnv(t, srv.URL)
	stdin = strings.NewReader("hi\n")
	defer func() { stdin = os.Stdin }()

	code, out, errS := runCLI2(t, srv.URL, "connect", "sb_1")
	if code != ExitOK {
		t.Fatalf("connect on a fleet deployment = %d, want 0 (stderr %q)", code, errS)
	}
	if !strings.Contains(out, "fleet-pty<hi\n>") {
		t.Fatalf("never got through the gate: %q / %q", out, errS)
	}
	if len(minted) != 1 || minted[0] != "7681" {
		t.Fatalf("preview-token calls = %v, want exactly one for port 7681", minted)
	}
}

// "Preview tokens unavailable" is a deployment fact, not a transient failure.
// The operator's own message must reach the user, and the generic 5xx "retry"
// hint must not, because retrying never fixes it.
func TestConnectSurfacesTheMintRefusal(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("GET /v1/sandboxes/sb_1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"terminal_url":"http://n/preview/sb_1/7681/"}`))
	})
	mux.HandleFunc("GET /v1/sandboxes/sb_1/preview-token", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"preview tokens unavailable: no API key configured"}`))
	})
	cpEnv(t, srv.URL)

	code, _, errS := runCLI2(t, srv.URL, "connect", "sb_1")
	if code != ExitAPI {
		t.Fatalf("code = %d, want %d", code, ExitAPI)
	}
	if !strings.Contains(errS, "preview tokens unavailable: no API key configured") {
		t.Fatalf("the platform's own message must survive: %q", errS)
	}
	if strings.Contains(errS, "retry") || strings.Contains(errS, "network problem") {
		t.Fatalf("a permanent deployment fact must not be hinted as transient: %q", errS)
	}
}

// A refusal is not a network problem, and must not be hinted as one.
func TestConnectGateRefusalIsNotHintedAsANetworkProblem(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/sandboxes/") {
			_, _ = w.Write([]byte(`{"terminal_url":"http://` + r.Host + `/preview/sb_1/7681/?ptok=t"}`))
			return
		}
		http.Error(w, "invalid or missing preview token", http.StatusForbidden)
	}))
	defer srv.Close()
	cpEnv(t, srv.URL)

	_, _, errS := runCLI2(t, srv.URL, "connect", "sb_1")
	if strings.Contains(errS, "network problem") || strings.Contains(errS, "plan/limit") {
		t.Fatalf("403 must not be hinted as a network or plan problem: %q", errS)
	}
	if !strings.Contains(errS, "sandbox is yours") {
		t.Fatalf("a gate refusal needs its own remedy: %q", errS)
	}
}

func TestPreviewPortIsReadFromTheLink(t *testing.T) {
	for in, want := range map[string]int{
		"http://n/preview/sb_1/7681/": 7681,
		"http://n/preview/sb_1/9999/": 9999, // follows the platform, not a constant here
		"https://7681-sb_1.sbx.test/": 7681,
		"https://8080-sb_1.sbx.test/": 8080,
		"http://n/somewhere/else":     terminalPort,
	} {
		u, err := url.Parse(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := previewPortOf(u); got != want {
			t.Errorf("previewPortOf(%q) = %d, want %d", in, got, want)
		}
	}
}
