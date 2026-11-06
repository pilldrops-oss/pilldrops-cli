package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"
)

// Teams need a person, and an API key is not one: `team login` gets a short
// session from someone approving a code in the dashboard, and the other team
// verbs use that session rather than the key.

func teamClient(c *cmdCtx) (*Client, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return nil, err
	}
	if cfg.TeamSession == "" {
		return nil, errors.New("not signed in for teams; run pilldrops team login")
	}
	return &Client{Base: cfg.TeamPortal, Key: cfg.TeamSession}, nil
}

func teamOrgFlag(fs *flag.FlagSet) func(*verbCall) {
	org := fs.String("org", "", "team org id (o:...); default your personal org")
	return func(v *verbCall) { v.cl.Org = *org }
}

func teamVerbs() []apiVerb {
	withOrg := func(fill func(*flag.FlagSet) func(*verbCall) error) func(*flag.FlagSet) func(*verbCall) error {
		return func(fs *flag.FlagSet) func(*verbCall) error {
			setOrg := teamOrgFlag(fs)
			prepare := func(*verbCall) error { return nil }
			if fill != nil {
				prepare = fill(fs)
			}
			return func(v *verbCall) error {
				setOrg(v)
				return prepare(v)
			}
		}
	}
	roleBody := func(fs *flag.FlagSet) func(*verbCall) error {
		role := fs.String("role", "member", "owner, admin or member")
		return func(v *verbCall) error {
			v.body = map[string]any{"email": v.pos[0], "role": *role}
			return nil
		}
	}
	return []apiVerb{
		{sub: "login", args: "[--portal URL] [--no-wait] [--device-code C]", flags: teamLoginFlags, client: anonymousClient},
		{sub: "logout", flags: teamLogoutFlags, client: anonymousClient},
		{sub: "orgs", method: "GET", path: "/v1/orgs", rows: "orgs", envelope: true, cols: []string{"id", "name", "role"}, client: teamClient},
		{sub: "org create", args: "<name>", method: "POST", path: "/v1/orgs", client: teamClient, flags: func(*flag.FlagSet) func(*verbCall) error {
			return func(v *verbCall) error {
				v.body = map[string]any{"name": v.pos[0]}
				return nil
			}
		}},
		{sub: "members", args: "[--org o:ID]", method: "GET", path: "/v1/members", rows: "members", envelope: true,
			cols: []string{"user_id", "email", "role"}, client: teamClient, flags: withOrg(nil)},
		{sub: "invite", args: "<email> [--role R] [--org o:ID]", method: "POST", path: "/v1/members", client: teamClient, flags: withOrg(roleBody)},
		{sub: "uninvite", args: "<email> [--org o:ID] --confirm", method: "DELETE", path: "/v1/members/invites/%s", confirm: true, client: teamClient, flags: withOrg(nil)},
		{sub: "role", args: "<user_id> <role> [--org o:ID]", method: "PATCH", path: "/v1/members/%s", client: teamClient, flags: withOrg(func(*flag.FlagSet) func(*verbCall) error {
			return func(v *verbCall) error {
				v.body = map[string]any{"role": v.pos[1]}
				return nil
			}
		})},
		{sub: "remove", args: "<user_id> [--org o:ID] --confirm", method: "DELETE", path: "/v1/members/%s", confirm: true, client: teamClient, flags: withOrg(nil)},
	}
}

// anonymousClient is for the verbs that run before there is any session.
func anonymousClient(*cmdCtx) (*Client, error) { return &Client{}, nil }

func teamLogoutFlags(*flag.FlagSet) func(*verbCall) error {
	return func(v *verbCall) error {
		v.run = func(v *verbCall) int {
			cfg, err := LoadConfig()
			if err != nil {
				return v.c.fail(err)
			}
			cfg.TeamSession, cfg.TeamPortal, cfg.TeamEmail = "", "", ""
			if err := SaveConfig(cfg); err != nil {
				return v.c.fail(err)
			}
			return v.respond([]byte(`{"status":"signed out of teams"}`))
		}
		return nil
	}
}

func teamLoginFlags(fs *flag.FlagSet) func(*verbCall) error {
	portal := fs.String("portal", "", "portal URL; default the API URL you logged in to")
	noWait := fs.Bool("no-wait", false, "print the approval link and device code, then exit")
	deviceCode := fs.String("device-code", "", "finish a login started with --no-wait")
	return func(v *verbCall) error {
		v.run = func(v *verbCall) int {
			base := strings.TrimRight(*portal, "/")
			if base == "" {
				logged, err := v.c.client()
				if err != nil {
					return v.c.fail(fmt.Errorf("name the portal with --portal: %w", err))
				}
				base = logged.Base
			}
			cl := &Client{Base: base}
			interval := 5
			code := *deviceCode
			if code == "" {
				raw, err := cl.Raw("POST", "/cli/device/start", []byte("{}"), "application/json")
				if err != nil {
					return v.c.fail(fmt.Errorf("this portal cannot sign in the CLI for teams: %w", err))
				}
				if *noWait {
					return v.respond(raw)
				}
				var start struct {
					DeviceCode      string `json:"device_code"`
					UserCode        string `json:"user_code"`
					VerificationURI string `json:"verification_uri"`
					Interval        int    `json:"interval"`
				}
				if err := json.Unmarshal(raw, &start); err != nil {
					return v.c.fail(err)
				}
				code, interval = start.DeviceCode, start.Interval
				fmt.Fprintf(v.c.stderr, "Open this link, sign in and approve the code %s:\n  %s\nWaiting for approval...\n", start.UserCode, start.VerificationURI)
			}
			return v.awaitTeamApproval(cl, code, interval)
		}
		return nil
	}
}

func (v *verbCall) awaitTeamApproval(cl *Client, code string, interval int) int {
	body, _ := json.Marshal(map[string]string{"device_code": code})
	for {
		raw, err := cl.Raw("POST", "/cli/device/token", body, "application/json")
		if err == nil {
			var granted struct {
				SessionToken string `json:"session_token"`
				Email        string `json:"email"`
				ExpiresIn    int    `json:"expires_in"`
			}
			if err := json.Unmarshal(raw, &granted); err != nil {
				return v.c.fail(err)
			}
			cfg, err := LoadConfig()
			if err != nil {
				return v.c.fail(err)
			}
			cfg.TeamSession, cfg.TeamPortal, cfg.TeamEmail = granted.SessionToken, cl.Base, granted.Email
			if err := SaveConfig(cfg); err != nil {
				return v.c.fail(err)
			}
			out, _ := json.Marshal(map[string]any{"status": "signed in for teams", "email": granted.Email, "expires_in": granted.ExpiresIn})
			return v.respond(out)
		}
		if !strings.Contains(err.Error(), "authorization_pending") {
			return v.c.fail(fmt.Errorf("sign-in did not finish: %w", err))
		}
		time.Sleep(time.Duration(max(interval, 1)) * time.Second)
	}
}
