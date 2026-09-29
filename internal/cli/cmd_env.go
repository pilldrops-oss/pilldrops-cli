package cli

import (
	"fmt"
	"net/url"
)

// vaultEntry mirrors the /v1/secrets wire shape.
type vaultEntry struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	AutoInject bool   `json:"auto_inject"`
	Last4      string `json:"last4"`
	Value      string `json:"value"`
	// Scope is which sandboxes this may be injected into: all | shared | private.
	Scope string `json:"scope"`
}

const envUsage = `usage: pilldrops env set NAME VALUE [--secret] [--auto]
       pilldrops env list [--scope all|shared|private]
       pilldrops env scope NAME all|shared|private
       pilldrops env rm NAME`

// cmdEnv manages the org vault: sealed secrets and readable variables, either
// referenced per create (secret_refs) or auto-injected into every create.
func cmdEnv(c *cmdCtx, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.stderr, envUsage)
		return ExitUsage
	}
	cl, err := c.client()
	if err != nil {
		return c.fail(err)
	}
	switch args[0] {
	case "set":
		secret, auto := false, false
		var pos []string
		for _, a := range args[1:] {
			switch a {
			case "--secret":
				secret = true
			case "--auto":
				auto = true
			default:
				pos = append(pos, a)
			}
		}
		if len(pos) != 2 {
			fmt.Fprintln(c.stderr, envUsage)
			return ExitUsage
		}
		var got vaultEntry
		body := map[string]any{"name": pos[0], "value": pos[1], "secret": secret, "auto_inject": auto}
		if err := cl.JSON("POST", "/v1/secrets", body, &got); err != nil {
			return c.fail(err)
		}
		if c.jsonOut {
			printJSON(c.stdout, got)
		} else if !c.quiet {
			fmt.Fprintf(c.stdout, "set %s (%s%s)\n", got.Name, got.Kind, autoSuffix(got.AutoInject))
		}
		return ExitOK
	case "scope":
		// The value is untouched; this only decides where the secret may go.
		if len(args) != 3 {
			fmt.Fprintln(c.stderr, envUsage)
			return ExitUsage
		}
		var got map[string]any
		if err := cl.JSON("POST", "/v1/secrets/"+url.PathEscape(args[1])+"/scope",
			map[string]any{"scope": args[2]}, &got); err != nil {
			return c.fail(err)
		}
		if c.jsonOut {
			printJSON(c.stdout, got)
		} else if !c.quiet {
			fmt.Fprintf(c.stdout, "%s scope %s\n", args[1], str(got["scope"]))
		}
		return ExitOK
	case "list", "ls":
		want := ""
		if len(args) == 3 && args[1] == "--scope" {
			want = args[2]
		}
		var list []vaultEntry
		if err := cl.JSON("GET", "/v1/secrets", nil, &list); err != nil {
			return c.fail(err)
		}
		if want != "" {
			// Filtered here rather than server-side: the list is one org's
			// secrets, already fetched, and a query parameter the server
			// ignored would silently show everything.
			kept := list[:0]
			for _, e := range list {
				if e.Scope == want || (want == "all" && e.Scope == "") {
					kept = append(kept, e)
				}
			}
			list = kept
		}
		if c.jsonOut {
			printJSON(c.stdout, list)
			return ExitOK
		}
		rows := [][]string{{"NAME", "VALUE", "AUTO", "SCOPE"}}
		for _, e := range list {
			val := e.Value
			if e.Kind == "secret" {
				val = "(sealed)"
			}
			autoCol := ""
			if e.AutoInject {
				autoCol = "yes"
			}
			scope := e.Scope
			if scope == "" {
				scope = "all"
			}
			rows = append(rows, []string{e.Name, val, autoCol, scope})
		}
		table(c.stdout, rows)
		return ExitOK
	case "rm", "delete":
		if len(args) != 2 {
			fmt.Fprintln(c.stderr, envUsage)
			return ExitUsage
		}
		if err := cl.JSON("DELETE", "/v1/secrets/"+url.PathEscape(args[1]), nil, nil); err != nil {
			return c.fail(err)
		}
		if c.jsonOut {
			printJSON(c.stdout, map[string]any{"removed": args[1]})
		} else if !c.quiet {
			fmt.Fprintf(c.stdout, "removed %s\n", args[1])
		}
		return ExitOK
	}
	fmt.Fprintln(c.stderr, envUsage)
	return ExitUsage
}

func autoSuffix(auto bool) string {
	if auto {
		return ", auto-injected"
	}
	return ""
}
