package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validConfig = `
version = "sandboxd/v1.2.0"
domain  = "sandbox.example.com"
admin_email = "ops@example.com"
[ssh]
user = "ubuntu"
[tls]
mode      = "files"
cert_file = "c.pem"
key_file  = "k.pem"
[control]
host       = "ctl.example.com"
private_ip = "10.0.0.10"
[[node]]
host = "10.0.0.21"
[[node]]
host    = "10.0.0.22"
runtime = "docker"
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cluster.toml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigFillsDefaults(t *testing.T) {
	path := writeConfig(t, validConfig)
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Zone != "default" || c.Nodes[0].Runtime != "auto" || c.Nodes[0].PrivateIP != "10.0.0.21" {
		t.Fatalf("defaults not filled: zone=%q runtime=%q private_ip=%q", c.Zone, c.Nodes[0].Runtime, c.Nodes[0].PrivateIP)
	}
	if want := filepath.Join(filepath.Dir(path), "c.pem"); c.TLS.CertFile != want {
		t.Fatalf("cert_file = %q, want it beside cluster.toml: %q", c.TLS.CertFile, want)
	}
	if got := c.Target(c.Control); got != "ubuntu@ctl.example.com" {
		t.Fatalf("Target = %q", got)
	}
}

func TestLoadConfigRefuses(t *testing.T) {
	cases := map[string]struct{ from, to, want string }{
		"unknown key":     {"[ssh]", "zones = \"eu\"\n[ssh]", `unknown setting "zones"`},
		"bad domain":      {`"sandbox.example.com"`, `"https://x.com"`, "bare hostname"},
		"quote in domain": {`"sandbox.example.com"`, `"a'b.com"`, "bare hostname"},
		"bad tag":         {`"sandboxd/v1.2.0"`, `"sandboxd/v1.2.0$(id)"`, "release tag"},
		"tls mode":        {`"files"`, `"acme"`, "tls.mode"},
		"name needs ip":   {"private_ip = \"10.0.0.10\"\n", "", "private_ip must be an IP"},
		"shared ip":       {`host    = "10.0.0.22"`, `host    = "10.0.0.21"`, "used twice"},
		"bad runtime":     {`"docker"`, `"kvm"`, "runtime must be"},
		"ipv6 private":    {`private_ip = "10.0.0.10"`, `private_ip = "fd00::10"`, "IPv4"},
		"ssh option host": {`host    = "10.0.0.22"`, `host    = "-oProxyCommand=id"`, "does not start with -"},
		"bad admin email": {`"ops@example.com"`, `"ops'@example.com"`, "admin_email"},
		"no nodes":        {"[[node]]", "[[nodex]]", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadConfig(writeConfig(t, strings.Replace(validConfig, tc.from, tc.to, 1)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestCloudflareNeedsItsToken(t *testing.T) {
	body := strings.Replace(validConfig, `mode      = "files"`, `mode = "cloudflare"
email = "a@example.com"
token_env = "PD_TEST_CF_TOKEN"`, 1)
	t.Setenv("PD_TEST_CF_TOKEN", "")
	if _, err := LoadConfig(writeConfig(t, body)); err == nil || !strings.Contains(err.Error(), "PD_TEST_CF_TOKEN") {
		t.Fatalf("err = %v, want the unset variable named", err)
	}
	t.Setenv("PD_TEST_CF_TOKEN", "tok")
	if _, err := LoadConfig(writeConfig(t, body)); err != nil {
		t.Fatal(err)
	}
}

func TestSecretsAreMadeOnceAndKept(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cluster.secrets")
	first, created, err := LoadSecrets(p)
	if err != nil || !created {
		t.Fatalf("first load: created=%v err=%v", created, err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
	for _, name := range secretNames {
		if len(first[name]) != 64 {
			t.Fatalf("%s = %q, want 32 bytes of hex", name, first[name])
		}
	}
	again, created, err := LoadSecrets(p)
	if err != nil || created {
		t.Fatalf("second load: created=%v err=%v", created, err)
	}
	for _, name := range secretNames {
		if again[name] != first[name] {
			t.Fatalf("%s changed between applies", name)
		}
	}

	// A secret added in a later release is generated without touching the rest.
	keep := strings.Repeat("ab", 32)
	if err := os.WriteFile(p, []byte("NODE_KEY_SECRET="+keep+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	grown, created, err := LoadSecrets(p)
	if err != nil || !created || grown["NODE_KEY_SECRET"] != keep || grown["VAULT_SECRET_KEY"] == "" {
		t.Fatalf("created=%v err=%v NODE_KEY_SECRET=%q", created, err, grown["NODE_KEY_SECRET"])
	}

	// A hand-edited value would reach shell, SQL and env files unquoted.
	if err := os.WriteFile(p, []byte("NODE_KEY_SECRET=it's\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadSecrets(p); err == nil || !strings.Contains(err.Error(), "64 hex") {
		t.Fatalf("err = %v, want a malformed secret refused", err)
	}

	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadSecrets(p); err == nil || !strings.Contains(err.Error(), "read by others") {
		t.Fatalf("err = %v, want a world-readable file refused", err)
	}
}

func TestMissingFiles(t *testing.T) {
	sums := "aa  orchestrator-linux-amd64\nbb  sandboxd-fleet-linux-amd64\ncc *envd-linux-amd64\ndd  pilldrops-linux-amd64\nee  source.tar.gz\n"
	got := missingFiles([]byte(sums))
	if len(got) != 1 || got[0] != "pilldrops-join-linux-amd64" {
		t.Fatalf("missing = %v", got)
	}
}

func testApplier(t *testing.T) *applier {
	t.Helper()
	cfg, err := LoadConfig(writeConfig(t, validConfig))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg.TLS.CertFile, cfg.TLS.KeyFile = filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	for _, f := range []string{cfg.TLS.CertFile, cfg.TLS.KeyFile} {
		if err := os.WriteFile(f, []byte("PEM"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sec, _, err := LoadSecrets(filepath.Join(t.TempDir(), "cluster.secrets"))
	if err != nil {
		t.Fatal(err)
	}
	return &applier{cfg: cfg, sec: sec, sums: []byte("aa  envd-linux-amd64\n"), out: io.Discard}
}

// bashCheck parses a rendered script without running it.
func bashCheck(t *testing.T, script string) {
	t.Helper()
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, out)
	}
}

func TestScriptsRender(t *testing.T) {
	a := testApplier(t)
	control, err := a.controlScript()
	if err != nil {
		t.Fatal(err)
	}
	bashCheck(t, control)
	for _, want := range []string{
		"NODE_KEY_SECRET=" + a.sec["NODE_KEY_SECRET"],
		"hostssl pilldrops pilldrops_node 10.0.0.22/32",
		"iptables -A PILLDROPS-IN -s 10.0.0.21 -p tcp --dport 5432 -j RETURN",
		"sandbox.example.com, *.sandbox.example.com {",
		"$ipt -A PILLDROPS-IN -p tcp --dport 9090 -j DROP",
		"for ipt in iptables ip6tables",
		"pilldrops-join-linux-amd64 source.tar.gz\"",
		"http://127.0.0.1:9090/dashboard/setup",
		"printf '%s' '" + a.sec["ADMIN_PASSWORD"] + "' > \"$stage/password\"",
	} {
		if !strings.Contains(control, want) {
			t.Errorf("control script lacks %q", want)
		}
	}
	if strings.Contains(control, "certbot") {
		t.Error("files mode should not run certbot")
	}
	// The console is claimed, and the ports closed, before anything is public.
	fw, orch, claim, caddy := strings.Index(control, `say "firewall"`), strings.Index(control, "systemctl restart pilldrops-orchestrator"),
		strings.Index(control, "/dashboard/setup"), strings.Index(control, "systemctl restart caddy")
	if !(fw < orch && orch < claim && claim < caddy) {
		t.Errorf("order: firewall %d, orchestrator %d, claim %d, caddy %d", fw, orch, claim, caddy)
	}

	a.cfg.TLS = TLS{Mode: "cloudflare", Email: "a@example.com", TokenEnv: "PD_TEST_CF"}
	t.Setenv("PD_TEST_CF", "it's-a-token")
	control, err = a.controlScript()
	if err != nil {
		t.Fatal(err)
	}
	bashCheck(t, control)
	if !strings.Contains(control, `'it'\''s-a-token'`) {
		t.Error("the cloudflare token is not quoted for the shell")
	}

	for _, runtime := range []string{"firecracker", "docker"} {
		n := a.cfg.Nodes[0]
		n.Runtime = runtime
		node, err := a.nodeScript(n, "tok'en")
		if err != nil {
			t.Fatal(err)
		}
		bashCheck(t, node)
		if !strings.Contains(node, "ADVERTISE_URL='http://10.0.0.21:18080'") || !strings.Contains(node, `'tok'\''en'`) {
			t.Errorf("%s node script does not carry the advertise URL and quoted token", runtime)
		}
		if strings.Contains(node, "archive/refs") || !strings.Contains(node, `files="envd-linux-amd64 pilldrops-linux-amd64 source.tar.gz"`) {
			t.Errorf("%s node script does not take the source from the signed release", runtime)
		}
		if got := strings.Contains(node, "provision-node.sh"); got != (runtime == "firecracker") {
			t.Errorf("%s node script: provision-node.sh present = %v", runtime, got)
		}
	}

	// A node that already joined gets the firewall and domain steps only.
	rejoin, err := a.nodeScript(a.cfg.Nodes[0], "")
	if err != nil {
		t.Fatal(err)
	}
	bashCheck(t, rejoin)
	for _, absent := range []string{"join.sh", "provision-node.sh", "apt-get"} {
		if strings.Contains(rejoin, absent) {
			t.Errorf("a joined node's script runs %s", absent)
		}
	}
	for _, want := range []string{"ip6tables -A PILLDROPS-NODE -p tcp --dport 18080 -j DROP", "SANDBOXD_SANDBOX_DOMAIN=sandbox.example.com"} {
		if !strings.Contains(rejoin, want) {
			t.Errorf("a joined node's script lacks %q", want)
		}
	}
}

func TestShellQuoteRoundTrips(t *testing.T) {
	for _, s := range []string{"plain", "it's", `$(id) "x" \n`, ""} {
		out, err := exec.Command("bash", "-c", "printf %s "+shellQuote(s)).Output()
		if err != nil || string(out) != s {
			t.Fatalf("%q came back as %q (%v)", s, out, err)
		}
	}
}

func TestCheckHostsResolvesRuntime(t *testing.T) {
	a := testApplier(t)
	a.ssh = func(_ context.Context, target, _ string, out io.Writer) error {
		if target == "ubuntu@10.0.0.21" {
			fmt.Fprintln(out, "sudo: unable to resolve host kvm: Name or service not known")
			fmt.Fprintln(out, "pilldrops-probe kvm=y joined=y")
		} else {
			fmt.Fprintln(out, "pilldrops-probe kvm=n joined=n")
		}
		return nil
	}
	if err := a.checkHosts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.cfg.Nodes[0].Runtime != "firecracker" || a.cfg.Nodes[1].Runtime != "docker" {
		t.Fatalf("runtimes = %s, %s", a.cfg.Nodes[0].Runtime, a.cfg.Nodes[1].Runtime)
	}
	if !a.cfg.Nodes[0].joined || a.cfg.Nodes[1].joined {
		t.Fatalf("joined = %v, %v; want it read from each host", a.cfg.Nodes[0].joined, a.cfg.Nodes[1].joined)
	}

	a.cfg.Nodes[1].Runtime = "firecracker"
	if err := a.checkHosts(context.Background()); err == nil || !strings.Contains(err.Error(), "/dev/kvm") {
		t.Fatalf("err = %v, want firecracker without KVM refused", err)
	}
}

// toServer sends every request to srv, whatever host it names.
type toServer struct{ srv *httptest.Server }

func (s toServer) RoundTrip(r *http.Request) (*http.Response, error) {
	u, _ := url.Parse(s.srv.URL)
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = u.Scheme, u.Host
	return http.DefaultTransport.RoundTrip(r)
}

func TestJoinNodeAndWaitNodes(t *testing.T) {
	a := testApplier(t)
	a.cfg.Nodes[0].Runtime, a.cfg.Nodes[1].Runtime = "docker", "docker"
	var joined map[string]bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+a.sec["ORCHESTRATOR_API_KEY"] {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/join-tokens":
			json.NewEncoder(w).Encode(map[string]string{"token": "jt_1"})
		case "GET /v1/nodes":
			var nodes []nodeView
			for u := range joined {
				nodes = append(nodes, nodeView{URL: u, Healthy: true})
			}
			json.NewEncoder(w).Encode(map[string]any{"nodes": nodes})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	a.http = &http.Client{Transport: toServer{srv}}

	var scripts []string
	a.ssh = func(_ context.Context, _, script string, _ io.Writer) error {
		scripts = append(scripts, script)
		return nil
	}
	if err := a.joinNode(context.Background(), a.cfg.Nodes[0]); err != nil {
		t.Fatal(err)
	}
	if len(scripts) != 1 || !strings.Contains(scripts[0], "'jt_1'") {
		t.Fatal("the node script did not carry the minted token")
	}

	pollEvery, nodeWait = time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { pollEvery, nodeWait = 5*time.Second, 5*time.Minute })
	joined = map[string]bool{advertiseURL(a.cfg.Nodes[0]): true}
	if err := a.waitNodes(context.Background()); err == nil || !strings.Contains(err.Error(), "10.0.0.22") {
		t.Fatalf("err = %v, want the missing node named", err)
	}
	joined[advertiseURL(a.cfg.Nodes[1])] = true
	if err := a.waitNodes(context.Background()); err != nil {
		t.Fatal(err)
	}
}
