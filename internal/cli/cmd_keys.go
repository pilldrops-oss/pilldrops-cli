package cli

import (
	"fmt"
	"net/url"
	"strings"
)

// cmdKeys drives GET|POST /v1/keys and DELETE /v1/keys/{id}. POST takes one
// field, name: the platform has no scopes and no expiry on an API key, so
// there is deliberately no flag implying otherwise.
func cmdKeys(c *cmdCtx, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.stderr, "usage: pilldrops keys create <name> | list | rm <id>")
		return ExitUsage
	}
	cl, err := c.client()
	if err != nil {
		return c.fail(err)
	}
	switch args[0] {
	case "list", "ls":
		var out []map[string]any
		if err := cl.JSON("GET", "/v1/keys", nil, &out); err != nil {
			return c.fail(err)
		}
		if c.jsonOut {
			printJSON(c.stdout, out)
			return ExitOK
		}
		rows := [][]string{{"ID", "NAME", "MASKED", "LAST_USED"}}
		for _, k := range out {
			rows = append(rows, []string{str(k["id"]), str(k["name"]), str(k["masked_key"]), str(k["last_used"])})
		}
		table(c.stdout, rows)
		return ExitOK

	case "create", "add":
		name := "key"
		if len(args) > 1 && strings.TrimSpace(args[1]) != "" {
			name = args[1]
		}
		var out map[string]any
		if err := cl.JSON("POST", "/v1/keys", map[string]string{"name": name}, &out); err != nil {
			return c.fail(err)
		}
		if c.jsonOut {
			printJSON(c.stdout, out)
			return ExitOK
		}
		// The plaintext comes back once and is never stored server-side, so it
		// goes to stdout on its own line for `KEY=$(pilldrops keys create ...)`.
		fmt.Fprintf(c.stdout, "%s\t%s\n%s\n", str(out["id"]), str(out["name"]), str(out["key"]))
		return ExitOK

	case "rm", "delete", "revoke":
		if len(args) < 2 {
			fmt.Fprintln(c.stderr, "usage: pilldrops keys rm <id>")
			return ExitUsage
		}
		var out map[string]any
		if err := cl.JSON("DELETE", "/v1/keys/"+url.PathEscape(args[1]), nil, &out); err != nil {
			return c.fail(err)
		}
		if out == nil {
			// The node answers {"status":"revoked"}; the portal answers 204
			// with no body. Say the same thing either way.
			out = map[string]any{"status": "revoked", "id": args[1]}
		}
		if c.jsonOut {
			printJSON(c.stdout, out)
		} else if !c.quiet {
			fmt.Fprintf(c.stdout, "revoked %s\n", args[1])
		}
		return ExitOK
	}
	fmt.Fprintf(c.stderr, "pilldrops keys: unknown subcommand %q\n", args[0])
	return ExitUsage
}
