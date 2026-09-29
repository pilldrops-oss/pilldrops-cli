package cli

import (
	"fmt"
	"net/http"
	"os"
	"runtime"
	"time"
)

// cmdDoctor runs the connectivity/auth checklist an agent (or a human) needs
// to unblock itself: which credentials resolved, whether the portal is
// reachable, and whether the key is actually accepted. Never prints secret
// values — only which sources are present.
func cmdDoctor(c *cmdCtx, _ []string) int {
	type check struct {
		Name   string `json:"name"`
		OK     bool   `json:"ok"`
		Detail string `json:"detail"`
	}
	var checks []check
	add := func(name string, ok bool, detail string) {
		checks = append(checks, check{name, ok, detail})
	}

	add("version", true, fmt.Sprintf("pilldrops %s (%s/%s)", Version, runtime.GOOS, runtime.GOARCH))

	// Credential sources (presence only, never values).
	var envs []string
	for _, n := range []string{"PILLDROPS_API_KEY", "PILLDROPS_API_URL", "SANDFORGE_API_KEY",
		"SANDFORGE_API_URL", "E2B_API_KEY", "SANDBOX_API_KEY"} {
		if os.Getenv(n) != "" {
			envs = append(envs, n)
		}
	}
	if len(envs) > 0 {
		add("env", true, fmt.Sprintf("set: %v", envs))
	} else {
		add("env", true, "no credential env vars set (config file may still apply)")
	}
	if _, err := LoadConfig(); err == nil {
		add("config", true, configPath())
	} else {
		add("config", false, configPath()+" not found — `pilldrops login` writes it")
	}

	url, key := resolveCreds(c.apiURL)
	if url == "" {
		url = DefaultAPIURL
	}
	add("api_url", true, url)
	add("api_key", key != "", map[bool]string{true: "resolved", false: "MISSING — run `pilldrops login --key <key>` or set PILLDROPS_API_KEY"}[key != ""])

	// Reachability (no auth) then an authenticated round-trip.
	hc := &http.Client{Timeout: 10 * time.Second}
	start := time.Now()
	resp, err := hc.Get(url + "/health")
	if err != nil {
		add("reachable", false, err.Error())
	} else {
		resp.Body.Close()
		add("reachable", resp.StatusCode == 200,
			fmt.Sprintf("GET /health → %d in %dms", resp.StatusCode, time.Since(start).Milliseconds()))
	}
	if key != "" {
		cl := &Client{Base: url, Key: key}
		if _, err := cl.Raw("GET", "/v1/templates", nil, ""); err != nil {
			add("auth", false, err.Error()+" — key rejected; re-run `pilldrops login`")
		} else {
			add("auth", true, "key accepted by /v1/templates")
		}
	}

	allOK := true
	for _, ch := range checks {
		if !ch.OK {
			allOK = false
		}
	}
	if c.jsonOut {
		printJSON(c.stdout, map[string]any{"ok": allOK, "checks": checks})
	} else {
		rows := [][]string{{"CHECK", "OK", "DETAIL"}}
		for _, ch := range checks {
			mark := "✓"
			if !ch.OK {
				mark = "✗"
			}
			rows = append(rows, []string{ch.Name, mark, ch.Detail})
		}
		table(c.stdout, rows)
	}
	if !allOK {
		return ExitAPI
	}
	return ExitOK
}
