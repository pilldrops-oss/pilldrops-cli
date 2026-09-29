package cli

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"
)

// pipedStdin reads standard input for the remote command. The whole of it: the
// node takes stdin as one field of the request, so the command cannot start
// until this returns, and the rule is about what may be waited on.
//
//	a terminal      never read — an interactive `exec <id> -- cat` returns
//	a regular file  read; `< file` ends, and reading it costs nothing
//	anything else   only with --stdin
//
// A pipe is the case that matters. `sleep 60 | pilldrops exec <id> -- echo hi`
// hung until the pipe closed, and so did every CI step, pipeline wrapper and
// `ssh host 'pilldrops exec …'` without -n, because a writer that is merely
// slow and one that will never write look identical from this side. Nothing
// here can tell whether the remote command wants input, so the caller says.
//
// A var-backed os.Stdin, so the cp tests can stub it too.
func pipedStdin(force bool) ([]byte, bool) {
	f, ok := stdin.(*os.File)
	if !ok {
		data, err := io.ReadAll(stdin) // a test stub: always "piped"
		return data, err == nil && len(data) > 0
	}
	st, err := f.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice != 0 {
		return nil, false
	}
	if !force && !st.Mode().IsRegular() {
		return nil, false
	}
	data, err := io.ReadAll(f)
	if err != nil || len(data) == 0 {
		return nil, false
	}
	return data, true
}

// shellQuote makes s safe as a single argument inside the `sh -c` string the node
// runs, so `exec … -- sh -c "a && b"` keeps its grouping instead of flattening.
// Simple tokens pass through; anything with shell-significant chars is single-
// quoted (embedded single quotes escaped the POSIX way).
func shellQuote(s string) string {
	// Quote only when a token carries whitespace or a shell operator that would
	// otherwise be interpreted/split. Bare tokens (paths, flags, KEY=VAL prefixes)
	// pass through unchanged.
	if s != "" && !strings.ContainsAny(s, " \t\n\"'\\$&|;<>()*?`") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// joinCmd shell-quotes each argv element and joins them into one command line.
func joinCmd(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = shellQuote(a)
	}
	return strings.Join(q, " ")
}

// cmdExec runs a command in the sandbox. Contract exception: the CLI's exit
// code is the REMOTE command's exit code, so agents can script it like ssh.
func cmdExec(c *cmdCtx, args []string) int {
	// Form: exec <id> [--cwd D] [--timeout S] -- <cmd...>
	dash := -1
	for i, a := range args {
		if a == "--" {
			dash = i
			break
		}
	}
	// `exec -- ls -la` is the id left off a command that is otherwise complete
	// — the one exec shape where the missing piece is something the tool can
	// find. A missing command line is not: nothing here can guess it.
	if dash == 0 && len(args) > 1 {
		if id, ok := c.askSandboxID("exec"); ok {
			args = append([]string{id}, args...)
			dash = 1
		}
	}
	if dash < 1 || dash == len(args)-1 {
		fmt.Fprintln(c.stderr, "usage: pilldrops exec <id> [--cwd D] [--timeout S] [--env K=V]... [--detach] -- <cmd...>")
		return ExitUsage
	}
	id := args[0]
	cwd, timeout := "", 0
	wantStdin := false         // --stdin: read a pipe and send it to the command
	detach := false            // --detach: background the command, return its pid
	env := map[string]string{} // per-command env (E2B SDK envs= parity), sent as an object
	opts := args[1:dash]
	for i := 0; i < len(opts); i++ {
		switch opts[i] {
		case "--detach", "-d":
			detach = true
		case "--stdin", "-i":
			wantStdin = true
		case "--cwd":
			if i+1 >= len(opts) {
				fmt.Fprintln(c.stderr, "pilldrops exec: --cwd needs a value")
				return ExitUsage
			}
			cwd = opts[i+1]
			i++
		case "--timeout":
			if i+1 >= len(opts) {
				fmt.Fprintln(c.stderr, "pilldrops exec: --timeout needs a value")
				return ExitUsage
			}
			if _, err := fmt.Sscanf(opts[i+1], "%d", &timeout); err != nil {
				fmt.Fprintln(c.stderr, "pilldrops exec: --timeout must be seconds")
				return ExitUsage
			}
			i++
		case "--env":
			if i+1 >= len(opts) {
				fmt.Fprintln(c.stderr, "pilldrops exec: --env needs KEY=VAL")
				return ExitUsage
			}
			k, v, ok := strings.Cut(opts[i+1], "=")
			if !ok || k == "" {
				fmt.Fprintf(c.stderr, "pilldrops exec: --env must be KEY=VAL, got %q\n", opts[i+1])
				return ExitUsage
			}
			env[k] = v
			i++
		default:
			fmt.Fprintf(c.stderr, "pilldrops exec: unknown option %q\n", opts[i])
			return ExitUsage
		}
	}
	cl, err := c.client()
	if err != nil {
		return c.fail(err)
	}
	var out struct {
		Stdout    string `json:"stdout"`
		Stderr    string `json:"stderr"`
		ExitCode  int    `json:"exit_code"`
		Truncated bool   `json:"truncated"`
	}
	req := map[string]any{"cmd": joinCmd(args[dash+1:]), "cwd": cwd, "timeout": timeout}
	if len(env) > 0 {
		req["env"] = env
	}
	if data, ok := pipedStdin(wantStdin); ok {
		req["stdin"] = base64.StdEncoding.EncodeToString(data)
	}
	if detach {
		// Fire-and-forget: the node backgrounds the command and returns just the
		// pid (no output is captured). Print the pid so it can be scripted.
		req["detach"] = true
		var d struct {
			Pid      int  `json:"pid"`
			Detached bool `json:"detached"`
		}
		if err := cl.JSON("POST", "/v1/sandboxes/"+id+"/exec", req, &d); err != nil {
			return c.fail(err)
		}
		if c.jsonOut {
			printJSON(c.stdout, map[string]any{"pid": d.Pid, "detached": true})
		} else {
			fmt.Fprintf(c.stdout, "%d\n", d.Pid)
		}
		return ExitOK
	}
	if err := cl.JSON("POST", "/v1/sandboxes/"+id+"/exec", req, &out); err != nil {
		return c.fail(err)
	}
	if c.jsonOut {
		printJSON(c.stdout, map[string]any{
			"stdout": out.Stdout, "stderr": out.Stderr,
			"exit_code": out.ExitCode, "truncated": out.Truncated,
		})
	} else {
		fmt.Fprint(c.stdout, out.Stdout)
		fmt.Fprint(c.stderr, out.Stderr)
	}
	return out.ExitCode
}
