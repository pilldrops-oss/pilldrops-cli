package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync/atomic"

	"golang.org/x/term"
)

const (
	shellPrompt = "pilldrops> "
	shellUsage  = "usage: pilldrops shell (needs a terminal on stdin and stdout, and no --json)"
)

// reexecPrefix is what goes ahead of a command's arguments when the shell runs
// it. Only the test binary needs any.
var reexecPrefix []string

// interactive reports whether a person is both typing and reading.
func (c *cmdCtx) interactive() bool {
	out, ok := c.stdout.(*os.File)
	return ok && c.promptable() && term.IsTerminal(int(out.Fd()))
}

func cmdShell(c *cmdCtx, args []string) int {
	// `shell <id>` was connect's alias before this command existed.
	if len(args) > 0 {
		return cmdConnect(c, args)
	}
	if !c.interactive() {
		fmt.Fprintln(c.stderr, shellUsage)
		return ExitUsage
	}
	if _, key := resolveCreds(c.apiURL); key == "" {
		if code := cmdLogin(c, nil); code != ExitOK {
			return code
		}
		// The server may have been changed at login's prompt, and must not lose to the flag.
		if cfg, err := LoadConfig(); err == nil && c.apiURL != "" {
			c.apiURL = cfg.APIURL
		}
	}

	// ^C reaches the running command too, being in our process group, and only
	// that command dies. At the prompt it just abandons the line being typed.
	var atPrompt atomic.Bool
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)
	defer signal.Stop(sigs)
	go func() {
		for range sigs {
			if atPrompt.Load() {
				fmt.Fprint(c.stdout, "\n"+shellPrompt)
			}
		}
	}()
	return shellLoop(stdin, c.stdout, c.stderr, c.runInChild, &atPrompt)
}

// runInChild runs one command in its own process rather than calling Run. The
// commands were written to own the process: connect's stdin pump cannot be
// interrupted and would eat the next line typed here, logs -f only ends by being
// killed, and rawMode exits on a signal. A child gives each its own lifetime.
func (c *cmdCtx) runInChild(args []string) int {
	exe, err := os.Executable()
	if err != nil {
		return c.fail(err)
	}
	argv := append([]string{}, reexecPrefix...)
	if c.apiURL != "" {
		argv = append(argv, "--api-url", c.apiURL)
	}
	if c.quiet {
		argv = append(argv, "--quiet")
	}
	cmd := exec.Command(exe, append(argv, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, c.stdout, c.stderr

	// Whatever a child does to the terminal, even dying in raw mode, ends here.
	if fd, ok := rawFD(); ok {
		if state, err := term.GetState(fd); err == nil {
			defer func() { _ = term.Restore(fd, state) }()
		}
	}
	err = cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if exit.ExitCode() < 0 { // killed by a signal: the cursor is still after the ^C
			fmt.Fprintln(c.stdout)
		}
		return exit.ExitCode()
	}
	if err != nil {
		return c.fail(err)
	}
	return ExitOK
}

// shellLoop reads and dispatches lines until exit, quit or end of input. A
// failing command has already said why, so the only thing left to do is go on.
func shellLoop(in io.Reader, out, errOut io.Writer, run func([]string) int, atPrompt *atomic.Bool) int {
	for {
		fmt.Fprint(out, shellPrompt)
		atPrompt.Store(true)
		line, err := readLine(in)
		atPrompt.Store(false)
		if err != nil {
			fmt.Fprintln(out) // ^D leaves the cursor at the prompt
			return ExitOK
		}
		args, err := shellSplit(line)
		if err != nil {
			fmt.Fprintf(errOut, "pilldrops: %v\n", err)
			continue
		}
		if len(args) == 0 {
			continue
		}
		verb := (&cmdCtx{}).takeGlobals(args)
		switch {
		case len(verb) == 0:
			fmt.Fprintln(errOut, "pilldrops: no command given; try help")
		case verb[0] == "exit" || verb[0] == "quit":
			return ExitOK
		case verb[0] == "help" || verb[0] == "--help" || verb[0] == "-h":
			usage(out)
			fmt.Fprintln(out, "\nin this shell: exit, quit or Ctrl-D leave; Ctrl-C stops the running command")
		case len(verb) == 1 && verb[0] == "shell":
			fmt.Fprintln(errOut, "pilldrops: already in the shell")
		default:
			if _, known := lookup(verb[0]); !known {
				fmt.Fprintf(errOut, "pilldrops: unknown command %q; try help\n", verb[0])
				continue
			}
			run(args)
		}
	}
}

// shellSplit splits a line as a POSIX shell would for quotes and backslashes,
// and expands nothing: no variables, no globs.
func shellSplit(line string) ([]string, error) {
	var words []string
	var word strings.Builder
	started := false
	var quote rune
	escaped := false
	for _, r := range line {
		switch {
		case escaped:
			// Inside double quotes a backslash only escapes what is special there.
			if quote == '"' && !strings.ContainsRune("\"\\$`", r) {
				word.WriteRune('\\')
			}
			word.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped, started = true, true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, started = r, true
		case r == ' ' || r == '\t':
			if started {
				words = append(words, word.String())
				word.Reset()
				started = false
			}
		default:
			word.WriteRune(r)
			started = true
		}
	}
	if escaped {
		return nil, errors.New("the line ends in a backslash")
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %c quote", quote)
	}
	if started {
		words = append(words, word.String())
	}
	return words, nil
}
