// Package cluster installs a PillDrops fleet from one cluster.toml: a control
// host (orchestrator, Postgres, Caddy) and the nodes that run sandboxes, set
// up over the operator's own ssh.
package cluster

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"

	"github.com/BurntSushi/toml"
)

// hostname is a lowercase DNS name; the domain goes unquoted into a Caddyfile,
// env files and certbot arguments.
var hostname = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z]{2,}$`)

// releaseTag is an engine release tag; it too goes into the host scripts.
var releaseTag = regexp.MustCompile(`^sandboxd/v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`)

// Config is cluster.toml. It holds only what the operator chooses; secrets
// live in cluster.secrets beside it, so this file can be committed.
type Config struct {
	Version string `toml:"version"`
	Domain  string `toml:"domain"`
	Zone    string `toml:"zone"`
	SSH     struct {
		User string `toml:"user"`
	} `toml:"ssh"`
	TLS     TLS    `toml:"tls"`
	Control Host   `toml:"control"`
	Nodes   []Node `toml:"node"`
}

// TLS chooses where the wildcard certificate for <domain> and *.<domain> comes
// from. "cloudflare" issues it with certbot over DNS-01; "files" uploads one
// the operator already has.
type TLS struct {
	Mode     string `toml:"mode"`
	Email    string `toml:"email"`
	TokenEnv string `toml:"token_env"`
	CertFile string `toml:"cert_file"`
	KeyFile  string `toml:"key_file"`
}

// Host is a machine reached over ssh at Host and by the rest of the fleet at
// PrivateIP, which defaults to Host.
type Host struct {
	Host      string `toml:"host"`
	PrivateIP string `toml:"private_ip"`
}

// Node is a host that runs sandboxes.
type Node struct {
	Host
	Runtime string `toml:"runtime"`
}

// Target is the ssh destination for h.
func (c *Config) Target(h Host) string {
	if c.SSH.User == "" {
		return h.Host
	}
	return c.SSH.User + "@" + h.Host
}

// LoadConfig reads and checks cluster.toml. An unknown key is an error, so a
// misspelt setting is not silently ignored.
func LoadConfig(path string) (*Config, error) {
	var c Config
	md, err := toml.DecodeFile(path, &c)
	if err != nil {
		return nil, err
	}
	if extra := md.Undecoded(); len(extra) > 0 {
		return nil, fmt.Errorf("%s: unknown setting %q", path, extra[0].String())
	}
	if err := c.check(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, f := range []*string{&c.TLS.CertFile, &c.TLS.KeyFile} {
		if *f != "" && !filepath.IsAbs(*f) {
			*f = filepath.Join(filepath.Dir(path), *f)
		}
	}
	return &c, nil
}

func (c *Config) check() error {
	if !releaseTag.MatchString(c.Version) {
		return fmt.Errorf("version must be a release tag such as sandboxd/v1.2.0, not %q", c.Version)
	}
	if !hostname.MatchString(c.Domain) {
		return fmt.Errorf("domain must be a bare hostname such as sandbox.example.com, not %q", c.Domain)
	}
	if c.Zone == "" {
		c.Zone = "default"
	}
	switch c.TLS.Mode {
	case "cloudflare":
		if c.TLS.Email == "" || c.TLS.TokenEnv == "" {
			return fmt.Errorf("tls mode cloudflare needs email and token_env")
		}
		if os.Getenv(c.TLS.TokenEnv) == "" {
			return fmt.Errorf("tls.token_env names %s, which is not set", c.TLS.TokenEnv)
		}
	case "files":
		if c.TLS.CertFile == "" || c.TLS.KeyFile == "" {
			return fmt.Errorf("tls mode files needs cert_file and key_file")
		}
	default:
		return fmt.Errorf("tls.mode must be cloudflare or files, not %q", c.TLS.Mode)
	}
	if err := fillPrivateIP(&c.Control, "control"); err != nil {
		return err
	}
	if len(c.Nodes) == 0 {
		return fmt.Errorf("add at least one [[node]]")
	}
	seen := map[string]bool{c.Control.PrivateIP: true}
	for i := range c.Nodes {
		n := &c.Nodes[i]
		if err := fillPrivateIP(&n.Host, fmt.Sprintf("node %d", i+1)); err != nil {
			return err
		}
		if seen[n.PrivateIP] {
			return fmt.Errorf("private_ip %s is used twice; the control host and each node need their own", n.PrivateIP)
		}
		seen[n.PrivateIP] = true
		switch n.Runtime {
		case "":
			n.Runtime = "auto"
		case "auto", "firecracker", "docker":
		default:
			return fmt.Errorf("node %s: runtime must be auto, firecracker or docker, not %q", n.Host.Host, n.Runtime)
		}
	}
	return nil
}

func fillPrivateIP(h *Host, what string) error {
	if h.Host == "" {
		return fmt.Errorf("%s needs host", what)
	}
	if h.PrivateIP == "" {
		h.PrivateIP = h.Host
	}
	if net.ParseIP(h.PrivateIP) == nil {
		return fmt.Errorf("%s: private_ip must be an IP address, not %q (set it when host is a name)", what, h.PrivateIP)
	}
	return nil
}
