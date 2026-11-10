package cluster

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

var (
	//go:embed control.sh
	controlScript string
	//go:embed node.sh
	nodeScript string

	scripts = template.Must(template.New("control").Funcs(template.FuncMap{"sq": shellQuote}).Parse(controlScript))
	_       = template.Must(scripts.New("node").Parse(nodeScript))
)

// Waits, as vars so a test can shorten them.
var (
	healthWait = 20 * time.Minute // includes the operator adding DNS records
	nodeWait   = 5 * time.Minute
	pollEvery  = 5 * time.Second
)

// Apply brings the fleet described by the cluster.toml at path to life, or
// back in line with it. Every step checks before it acts, so it can run again.
func Apply(ctx context.Context, path string, out io.Writer) error {
	cfg, err := LoadConfig(path)
	if err != nil {
		return err
	}
	secretsPath := filepath.Join(filepath.Dir(path), "cluster.secrets")
	sec, created, err := LoadSecrets(secretsPath)
	if err != nil {
		return err
	}
	if created {
		fmt.Fprintf(out, "── wrote new secrets to %s; back it up, a fleet cannot be recovered without it\n", secretsPath)
	}
	a := &applier{cfg: cfg, sec: sec, out: out, ssh: sshRun}

	fmt.Fprintln(out, "── checking hosts")
	if err := a.checkHosts(ctx); err != nil {
		return err
	}
	fmt.Fprintf(out, "── checking the signature of %s\n", cfg.Version)
	sums, err := fetchSums(cfg.Version)
	if err != nil {
		return err
	}
	a.sums = sums

	fmt.Fprintf(out, "── control host %s\n", cfg.Control.Host)
	script, err := a.controlScript()
	if err != nil {
		return err
	}
	if err := a.ssh(ctx, cfg.Target(cfg.Control), script, out); err != nil {
		return fmt.Errorf("control host %s: %w", cfg.Control.Host, err)
	}

	fmt.Fprintf(out, "── waiting for https://%s\n   point these DNS records at %s if they do not already:\n      %s\n      *.%s\n",
		cfg.Domain, cfg.Control.Host[strings.LastIndex(cfg.Control.Host, "@")+1:], cfg.Domain, cfg.Domain)
	if err := a.waitHealthy(ctx); err != nil {
		return err
	}

	registered, err := a.nodes(ctx)
	if err != nil {
		return err
	}
	for _, n := range cfg.Nodes {
		if _, ok := registered[advertiseURL(n)]; ok {
			fmt.Fprintf(out, "── node %s already joined\n", n.Host.Host)
			continue
		}
		fmt.Fprintf(out, "── node %s (%s)\n", n.Host.Host, n.Runtime)
		if err := a.joinNode(ctx, n); err != nil {
			return fmt.Errorf("node %s: %w", n.Host.Host, err)
		}
	}

	fmt.Fprintln(out, "── checking every node reports healthy")
	if err := a.waitNodes(ctx); err != nil {
		return err
	}
	fmt.Fprintf(out, `
✓ The fleet is running: %d node(s).

  Dashboard   https://%s/   (the first visit creates the admin account; open it now)
  Operator    ORCHESTRATOR_API_KEY in %s
`, len(cfg.Nodes), cfg.Domain, secretsPath)
	return nil
}

type applier struct {
	cfg  *Config
	sec  Secrets
	sums []byte
	out  io.Writer
	ssh  func(ctx context.Context, target, script string, out io.Writer) error
	http *http.Client
}

// sshRun pipes script into a root shell on target. The script, secrets
// included, travels on stdin, so it never appears in a process list.
func sshRun(ctx context.Context, target, script string, out io.Writer) error {
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=15", target, "sudo", "bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}

// checkHosts reaches every host before anything is changed, and resolves each
// node's runtime "auto" to what the host can run.
func (a *applier) checkHosts(ctx context.Context) error {
	if err := a.ssh(ctx, a.cfg.Target(a.cfg.Control), "grep -q 'ID=ubuntu' /etc/os-release", io.Discard); err != nil {
		return fmt.Errorf("control host %s: ssh with sudo to an Ubuntu host failed: %w", a.cfg.Control.Host, err)
	}
	for i := range a.cfg.Nodes {
		n := &a.cfg.Nodes[i]
		var kvm bytes.Buffer
		if err := a.ssh(ctx, a.cfg.Target(n.Host), "grep -q 'ID=ubuntu' /etc/os-release && { [ -e /dev/kvm ] && echo kvm || echo none; }", &kvm); err != nil {
			return fmt.Errorf("node %s: ssh with sudo to an Ubuntu host failed: %w", n.Host.Host, err)
		}
		hasKVM := strings.TrimSpace(kvm.String()) == "kvm"
		switch {
		case n.Runtime == "firecracker" && !hasKVM:
			return fmt.Errorf("node %s: runtime firecracker needs /dev/kvm, which this host does not have", n.Host.Host)
		case n.Runtime == "auto" && hasKVM:
			n.Runtime = "firecracker"
		case n.Runtime == "auto":
			n.Runtime = "docker"
		}
	}
	return nil
}

func (a *applier) base() map[string]any {
	return map[string]any{
		"Tag":         a.cfg.Version,
		"Domain":      a.cfg.Domain,
		"Zone":        a.cfg.Zone,
		"Secrets":     a.sec,
		"SumsB64":     base64.StdEncoding.EncodeToString(a.sums),
		"ReleaseBase": releaseBase + a.cfg.Version + "/",
		"SourceURL":   "https://github.com/" + releaseRepo + "/archive/refs/tags/" + a.cfg.Version + ".tar.gz",
	}
}

func (a *applier) controlScript() (string, error) {
	d := a.base()
	d["Control"] = a.cfg.Control
	d["Nodes"] = a.cfg.Nodes
	d["TLS"] = a.cfg.TLS
	if a.cfg.TLS.Mode == "cloudflare" {
		d["CloudflareToken"] = os.Getenv(a.cfg.TLS.TokenEnv)
	} else {
		for key, file := range map[string]string{"CertB64": a.cfg.TLS.CertFile, "KeyB64": a.cfg.TLS.KeyFile} {
			b, err := os.ReadFile(file)
			if err != nil {
				return "", err
			}
			d[key] = base64.StdEncoding.EncodeToString(b)
		}
	}
	return render("control", d)
}

func (a *applier) nodeScript(n Node, token string) (string, error) {
	d := a.base()
	d["Runtime"] = n.Runtime
	d["ControlIP"] = a.cfg.Control.PrivateIP
	d["Token"] = token
	d["OrchestratorURL"] = "https://" + a.cfg.Domain
	d["AdvertiseURL"] = advertiseURL(n)
	return render("node", d)
}

func render(name string, data any) (string, error) {
	var b strings.Builder
	if err := scripts.ExecuteTemplate(&b, name, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

// advertiseURL is how the control plane reaches a node, and how apply
// recognises one that has already joined.
func advertiseURL(n Node) string { return "http://" + n.PrivateIP + ":18080" }

func (a *applier) joinNode(ctx context.Context, n Node) error {
	var tok struct {
		Token string `json:"token"`
	}
	if err := a.api(ctx, "POST", "/v1/join-tokens", &tok); err != nil {
		return fmt.Errorf("mint a join token: %w", err)
	}
	script, err := a.nodeScript(n, tok.Token)
	if err != nil {
		return err
	}
	return a.ssh(ctx, a.cfg.Target(n.Host), script, a.out)
}

func (a *applier) waitHealthy(ctx context.Context) error {
	url := "https://" + a.cfg.Domain + "/health"
	return poll(ctx, healthWait, func() (bool, error) {
		resp, err := a.client().Get(url)
		if err != nil {
			return false, nil
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK, nil
	}, func() error {
		return fmt.Errorf("%s did not answer within %v; check the DNS records above and `journalctl -u caddy -u pilldrops-orchestrator` on the control host", url, healthWait)
	})
}

type nodeView struct {
	URL     string `json:"url"`
	Healthy bool   `json:"healthy"`
}

func (a *applier) nodes(ctx context.Context) (map[string]nodeView, error) {
	var page struct {
		Nodes []nodeView `json:"nodes"`
	}
	if err := a.api(ctx, "GET", "/v1/nodes", &page); err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	m := map[string]nodeView{}
	for _, n := range page.Nodes {
		m[n.URL] = n
	}
	return m, nil
}

func (a *applier) waitNodes(ctx context.Context) error {
	var unhealthy []string
	return poll(ctx, nodeWait, func() (bool, error) {
		got, err := a.nodes(ctx)
		if err != nil {
			return false, nil // a node restarting can briefly fail the listing
		}
		unhealthy = unhealthy[:0]
		for _, n := range a.cfg.Nodes {
			if !got[advertiseURL(n)].Healthy {
				unhealthy = append(unhealthy, n.Host.Host)
			}
		}
		return len(unhealthy) == 0, nil
	}, func() error {
		return fmt.Errorf("not healthy after %v: %s; check `journalctl -u sandboxd` on each", nodeWait, strings.Join(unhealthy, ", "))
	})
}

func (a *applier) api(ctx context.Context, method, path string, into any) error {
	req, err := http.NewRequestWithContext(ctx, method, "https://"+a.cfg.Domain+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.sec["ORCHESTRATOR_API_KEY"])
	resp, err := a.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, into)
}

func (a *applier) client() *http.Client {
	if a.http != nil {
		return a.http
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func poll(ctx context.Context, limit time.Duration, done func() (bool, error), timeout func() error) error {
	deadline := time.Now().Add(limit)
	for {
		ok, err := done()
		if err != nil || ok {
			return err
		}
		if time.Now().After(deadline) {
			return timeout()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollEvery):
		}
	}
}

// shellQuote makes s one single-quoted shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
