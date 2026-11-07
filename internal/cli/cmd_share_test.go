package cli

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

var shareTestUpgrader = websocket.Upgrader{}

// fakeShareServer is the API and the ingress in one, driven by the test.
type fakeShareServer struct {
	srv       *httptest.Server
	mu        sync.Mutex
	ctrl      *websocket.Conn
	ctrlCount atomic.Int32
	hello     map[string]any
	dataConns chan *websocket.Conn
	tokenGone atomic.Bool
	deleted   atomic.Bool
	dropFirst atomic.Bool
}

func newFakeShareServer(t *testing.T, shareLinks bool) *fakeShareServer {
	t.Helper()
	f := &fakeShareServer{dataConns: make(chan *websocket.Conn, 8)}
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, code int) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "sh_1", "name": "quiet-otter-aaaa1111", "url": "https://quiet-otter-aaaa1111.share.test",
			"tunnel_url": "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/tunnel", "tunnel_token": "tok",
			"expires_at": time.Now().Add(time.Hour).Unix(),
		})
	}
	mux.HandleFunc("GET /v1/capabilities", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"share_links": shareLinks, "features": []string{}})
	})
	mux.HandleFunc("POST /v1/shares", func(w http.ResponseWriter, _ *http.Request) { reply(w, http.StatusCreated) })
	mux.HandleFunc("POST /v1/shares/{id}/token", func(w http.ResponseWriter, _ *http.Request) {
		if f.tokenGone.Load() {
			w.WriteHeader(http.StatusGone)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "this share link has ended: expired", "code": "share_ended", "reason": "expired"})
			return
		}
		reply(w, http.StatusOK)
	})
	mux.HandleFunc("DELETE /v1/shares/{id}", func(w http.ResponseWriter, _ *http.Request) {
		f.deleted.Store(true)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "closed"})
	})
	mux.HandleFunc("GET /tunnel", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		c, err := shareTestUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		f.ctrlCount.Add(1)
		var hello map[string]any
		_ = c.ReadJSON(&hello)
		if f.dropFirst.CompareAndSwap(true, false) {
			_ = c.Close()
			return
		}
		f.mu.Lock()
		f.ctrl, f.hello = c, hello
		f.mu.Unlock()
	})
	mux.HandleFunc("GET /tunnel/conn/{id}", func(w http.ResponseWriter, r *http.Request) {
		c, err := shareTestUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		f.dataConns <- c
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeShareServer) control(t *testing.T) *websocket.Conn {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		c := f.ctrl
		f.mu.Unlock()
		if c != nil {
			return c
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the CLI never connected")
	return nil
}

func (f *fakeShareServer) open(t *testing.T, id string) *wsConn {
	t.Helper()
	if err := f.control(t).WriteJSON(map[string]any{"type": "open", "id": id}); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-f.dataConns:
		return &wsConn{ws: c}
	case <-time.After(5 * time.Second):
		t.Fatal("the CLI opened no data connection")
		return nil
	}
}

func listen(t *testing.T, network, addr string) net.Listener {
	t.Helper()
	l, err := net.Listen(network, addr)
	if err != nil {
		t.Skipf("cannot listen on %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func echoOnce(l net.Listener) {
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, c)
	}()
}

func runSession(t *testing.T, f *fakeShareServer, port int) (context.CancelFunc, chan string) {
	t.Helper()
	cl := &Client{Base: f.srv.URL, Key: "k"}
	ss := &shareSession{
		cl: cl, id: "sh_1", port: port, stderr: io.Discard,
		tunnelURL: "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/tunnel", token: "tok",
		backoffMin: 5 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	out := make(chan string, 1)
	go func() { out <- ss.run(ctx) }()
	return cancel, out
}

func portOf(l net.Listener) int { return l.Addr().(*net.TCPAddr).Port }

func TestSharePipesBytesAndSaysHello(t *testing.T) {
	f := newFakeShareServer(t, true)
	l := listen(t, "tcp", "127.0.0.1:0")
	echoOnce(l)
	runSession(t, f, portOf(l))
	conn := f.open(t, strings.Repeat("a", 32))
	defer conn.Close()
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("echo = %q %v", buf, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hello["type"] != "hello" || int(f.hello["port"].(float64)) != portOf(l) {
		t.Fatalf("hello = %v", f.hello)
	}
}

func TestShareReachesAnAppBoundOnlyToIPv6Loopback(t *testing.T) {
	f := newFakeShareServer(t, true)
	l := listen(t, "tcp6", "[::1]:0")
	echoOnce(l)
	runSession(t, f, portOf(l))
	conn := f.open(t, strings.Repeat("b", 32))
	defer conn.Close()
	_, _ = conn.Write([]byte("v6"))
	buf := make([]byte, 2)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "v6" {
		t.Fatalf("echo = %q %v", buf, err)
	}
}

func TestShareClosesTheDataConnectionWhenTheAppIsDown(t *testing.T) {
	f := newFakeShareServer(t, true)
	l := listen(t, "tcp", "127.0.0.1:0")
	port := portOf(l)
	_ = l.Close()
	runSession(t, f, port)
	conn := f.open(t, strings.Repeat("c", 32))
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("read = %v, want EOF at once", err)
	}
}

func TestShareOnlyEverDialsItsOwnPort(t *testing.T) {
	f := newFakeShareServer(t, true)
	chosen := listen(t, "tcp", "127.0.0.1:0")
	other := listen(t, "tcp", "127.0.0.1:0")
	var otherHits atomic.Int32
	go func() {
		for {
			c, err := other.Accept()
			if err != nil {
				return
			}
			otherHits.Add(1)
			_ = c.Close()
		}
	}()
	echoOnce(chosen)
	runSession(t, f, portOf(chosen))
	ctrl := f.control(t)
	// A hostile server names the other port every way the protocol could carry it.
	otherPort := strconv.Itoa(portOf(other))
	for _, msg := range []map[string]any{
		{"type": "open", "id": strings.Repeat("d", 32), "port": portOf(other), "host": "127.0.0.1:" + otherPort, "addr": "127.0.0.1:" + otherPort},
		{"type": "open", "id": "../../" + otherPort},
		{"type": "connect", "target": "127.0.0.1:" + otherPort},
	} {
		if err := ctrl.WriteJSON(msg); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if n := otherHits.Load(); n != 0 {
		t.Fatalf("the other port was reached %d times", n)
	}
}

func TestShareIgnoresABadOpenID(t *testing.T) {
	f := newFakeShareServer(t, true)
	l := listen(t, "tcp", "127.0.0.1:0")
	runSession(t, f, portOf(l))
	if err := f.control(t).WriteJSON(map[string]any{"type": "open", "id": "../../evil"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.dataConns:
		t.Fatal("a data connection was opened for a malformed id")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestShareReconnectsAfterADrop(t *testing.T) {
	f := newFakeShareServer(t, true)
	f.dropFirst.Store(true)
	l := listen(t, "tcp", "127.0.0.1:0")
	runSession(t, f, portOf(l))
	f.control(t)
	if n := f.ctrlCount.Load(); n < 2 {
		t.Fatalf("connected %d times, want a reconnect", n)
	}
}

func TestShareStopsWhenTheServerSaysItEnded(t *testing.T) {
	f := newFakeShareServer(t, true)
	l := listen(t, "tcp", "127.0.0.1:0")
	_, out := runSession(t, f, portOf(l))
	_ = f.control(t).WriteJSON(map[string]any{"type": "closed", "reason": "blocked"})
	select {
	case reason := <-out:
		if reason != "blocked" {
			t.Fatalf("reason = %q", reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("it kept going")
	}
}

func TestShareStopsWhenTheLinkEndedWhileOffline(t *testing.T) {
	f := newFakeShareServer(t, true)
	l := listen(t, "tcp", "127.0.0.1:0")
	_, out := runSession(t, f, portOf(l))
	ctrl := f.control(t)
	f.tokenGone.Store(true)
	_ = ctrl.Close()
	select {
	case reason := <-out:
		if reason != "expired" {
			t.Fatalf("reason = %q", reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("it retried forever")
	}
}

func TestShareCommandRefusesAServerWithoutIt(t *testing.T) {
	f := newFakeShareServer(t, false)
	code, _, errS := authedCLI(t, f.srv, "share", "3000")
	if code == ExitOK || !strings.Contains(errS, "does not offer share links") {
		t.Fatalf("code %d, stderr %q", code, errS)
	}
}

func TestShareCommandChecksItsArguments(t *testing.T) {
	f := newFakeShareServer(t, true)
	for _, args := range [][]string{
		{"share"}, {"share", "nope"}, {"share", "0"}, {"share", "70000"},
		{"share", "3000", "--ttl", "soon"}, {"share", "3000", "--bad"},
	} {
		if code, _, _ := authedCLI(t, f.srv, args...); code != ExitUsage {
			t.Errorf("%v = %d, want usage", args, code)
		}
	}
}

// lockedBuffer is a stderr the session writes from its own goroutine.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestShareSaysWhenTheTunnelIsBack(t *testing.T) {
	for _, drop := range []bool{false, true} {
		f := newFakeShareServer(t, true)
		f.dropFirst.Store(drop)
		l := listen(t, "tcp", "127.0.0.1:0")
		var stderr lockedBuffer
		ss := &shareSession{
			cl: &Client{Base: f.srv.URL, Key: "k"}, id: "sh_1", port: portOf(l), stderr: &stderr,
			tunnelURL: "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/tunnel", token: "tok",
			backoffMin: 5 * time.Millisecond,
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { ss.run(ctx); close(done) }()
		f.control(t)
		time.Sleep(50 * time.Millisecond)
		cancel()
		<-done
		want := 0
		if drop {
			want = 1
		}
		if got := strings.Count(stderr.String(), "tunnel reconnected"); got != want {
			t.Errorf("drop %v: stderr %q", drop, stderr.String())
		}
	}
}

func TestShareCommandExitsNonZeroWhenTheServerEndsTheLink(t *testing.T) {
	f := newFakeShareServer(t, true)
	type result struct {
		code int
		errS string
	}
	out := make(chan result, 1)
	go func() {
		code, _, errS := authedCLI(t, f.srv, "share", "3000")
		out <- result{code, errS}
	}()
	_ = f.control(t).WriteJSON(map[string]any{"type": "closed", "reason": "blocked"})
	select {
	case r := <-out:
		if r.code != ExitAPI || !strings.Contains(r.errS, "the link has ended: blocked") {
			t.Fatalf("code %d, stderr %q", r.code, r.errS)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the command kept running")
	}
}
