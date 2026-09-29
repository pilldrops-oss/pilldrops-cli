package cli

import (
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// Prompting is the friendly half of the contract stated in this package's
// doc comment (config.go): "no prompts without a TTY". That guarantee is not
// weakened here, it is completed. A missing required argument still prints
// usage and exits 2 for everything automated — same output, same status — and
// only a human at a keyboard is asked for it instead.
//
// One gate, promptable(), decides that for every verb, so there is exactly one
// place the rule can be broken and exactly one place to read it.
//
// Questions and their answers go to STDERR. stdout is the data channel:
// `cp sb:/f -` writes file bytes there, `logs` pipes lines there, the tables
// are parsed there and --json puts its document there, so a question mixed
// into it would corrupt whatever is reading. stderr is already where this CLI
// puts its hints and its notes to a human, and a terminal shows both anyway.

// promptable reports whether a question may be asked at all.
//
//   - --json is a machine contract, so it never prompts, terminal or not. Even
//     on stderr: the two streams land in the same place on a terminal, and the
//     point of --json is that the answer comes from the caller, not a person.
//   - stdin must be a terminal. rawFD is the check `connect` already uses to
//     decide whether it has a terminal to drive; it reads the package `stdin`
//     var, so a test can hand it a real pty and get the real answer.
func (c *cmdCtx) promptable() bool {
	if c.jsonOut {
		return false
	}
	_, isTTY := rawFD()
	return isTTY
}

// ask puts one question on stderr and reads the answer. def is what a bare
// return means; ok is false when the question was not answered at all (^D, or
// an empty line where there is no default), and the caller then does exactly
// what it did before — usage, exit 2.
//
// ^C needs nothing here on purpose: this never touches the terminal's state
// (the answer is typed, so it must echo), so the default SIGINT disposition
// kills the process with the terminal exactly as it was found and with nothing
// done. Raw mode is `connect`'s business, and it starts after this returns.
func (c *cmdCtx) ask(question, def string) (string, bool) {
	if def != "" {
		fmt.Fprintf(c.stderr, "%s [%s]: ", question, def)
	} else {
		fmt.Fprintf(c.stderr, "%s: ", question)
	}
	line, err := readLine(stdin)
	if err != nil {
		fmt.Fprintln(c.stderr) // ^D leaves the cursor mid-line
		return "", false
	}
	if line = strings.TrimSpace(line); line == "" {
		return def, def != ""
	}
	return line, true
}

// readLine reads one line, a byte at a time on purpose: a buffered reader
// would swallow whatever follows the newline, and `connect` starts pumping the
// rest of stdin into the remote shell the moment this returns.
func readLine(r io.Reader) (string, error) {
	var line []byte
	var b [1]byte
	for {
		n, err := r.Read(b[:])
		if n > 0 {
			if b[0] == '\n' {
				return strings.TrimSuffix(string(line), "\r"), nil
			}
			line = append(line, b[0])
		}
		if err != nil {
			if len(line) > 0 {
				return string(line), nil // a last line with no newline
			}
			return "", err
		}
	}
}

// askSandboxID fills in a missing sandbox id, and answers the question itself
// wherever it can: with one sandbox that one is the default, so return is the
// answer; with several they are listed and picked by number. A literal id or
// slug is always accepted too — the list is a convenience, not the only way in.
//
// ("", false) when it must not ask (--json, no terminal), when it cannot make
// the question answerable (not logged in, no sandboxes, the listing failed),
// or when nobody answered.
func (c *cmdCtx) askSandboxID(verb string) (string, bool) {
	if !c.promptable() {
		return "", false
	}
	cl, err := c.client()
	if err != nil {
		// Say why rather than degrading to a bare usage line. A human who
		// typed this at a keyboard and got only usage concludes the tool has
		// no interactive mode at all — which is exactly what happened the
		// first time someone tried it against an unreachable deployment.
		fmt.Fprintf(c.stderr, "pilldrops %s: cannot offer a sandbox (%v)\n", verb, err)
		return "", false
	}
	var page struct {
		Sandboxes []map[string]any `json:"sandboxes"`
	}
	if err := cl.JSON("GET", "/v1/sandboxes", nil, &page); err != nil {
		fmt.Fprintf(c.stderr, "pilldrops %s: cannot list sandboxes to offer one (%v)\n", verb, err)
		return "", false
	}
	// An id typed blind into an empty account is a 404 either way, so there is
	// no question worth asking — but say why. Falling silently through to a
	// bare usage line is what made the first person to try this conclude the
	// tool had no interactive mode at all: their account simply had nothing in
	// it, and nothing said so.
	if len(page.Sandboxes) == 0 {
		fmt.Fprintf(c.stderr, "pilldrops %s: you have no sandboxes yet \u2014 create one with `pilldrops sandbox create`\n", verb)
		return "", false
	}
	label := func(sb map[string]any) string {
		return strings.TrimRight(strings.Join([]string{
			str(sb["id"]), str(sb["template"]), sandboxState(sb),
		}, "  "), " ")
	}
	if len(page.Sandboxes) == 1 {
		sb := page.Sandboxes[0]
		fmt.Fprintf(c.stderr, "pilldrops %s: the only sandbox is %s\n", verb, label(sb))
		return c.ask("sandbox", str(sb["id"]))
	}
	fmt.Fprintf(c.stderr, "pilldrops %s: which sandbox?\n", verb)
	for i, sb := range page.Sandboxes {
		fmt.Fprintf(c.stderr, "  %d  %s\n", i+1, label(sb))
	}
	// Three goes, not a loop: a terminal that keeps answering out of range is
	// something stuck, and hanging on it forever is worse than exiting 2.
	for try := 0; try < 3; try++ {
		ans, ok := c.ask(fmt.Sprintf("number 1-%d, or an id", len(page.Sandboxes)), "")
		if !ok {
			return "", false
		}
		n, err := strconv.Atoi(ans)
		if err != nil {
			return ans, true // an id or a slug, typed out
		}
		if n >= 1 && n <= len(page.Sandboxes) {
			return str(page.Sandboxes[n-1]["id"]), true
		}
		fmt.Fprintf(c.stderr, "  there is no %d\n", n)
	}
	return "", false
}

// wantID completes a verb whose one argument is a sandbox id. It is called at
// exactly the points that print usage today, so off a terminal args comes back
// untouched and the usage error that follows is the one that always fired.
func (c *cmdCtx) wantID(verb string, args []string) []string {
	if len(args) != 0 {
		return args
	}
	if id, ok := c.askSandboxID(verb); ok {
		return []string{id}
	}
	return args
}

// askCpOther completes `cp` when only one of its two sides was typed. The
// direction is settled by the side that IS there, and the default is where the
// bytes would obviously land, so return is an answer.
func (c *cmdCtx) askCpOther(given string) (string, bool) {
	// "-" is stdin or stdout; it says nothing about which side the sandbox is
	// on, so there is no question to ask that has one right answer.
	if !c.promptable() || given == "-" {
		return "", false
	}
	if _, p, remote := sandboxPath(given); remote {
		base := path.Base(homePath(p))
		if base == "." || base == "/" {
			return "", false
		}
		return c.ask("destination", base)
	}
	id, ok := c.askSandboxID("cp")
	if !ok {
		return "", false
	}
	return c.ask("destination", id+":~/"+filepath.Base(given))
}
