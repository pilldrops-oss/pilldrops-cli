package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// apiVerb declares a subcommand that is, in the common case, one request: the
// groups below are thin over the API and differ only in route and table shape,
// so they share one parser, one error path and one renderer.
type apiVerb struct {
	sub string // "" is the group's bare form
	// args is the usage tail; each "<x>" is a required positional.
	args     string
	optional int    // further positionals allowed after the required ones
	method   string // empty for a verb with run
	path     string // each %s takes the next positional
	confirm  bool
	rows     string   // response key holding a list; --json prints the list
	cols     []string // the columns of that list as a table
	envelope bool     // --json keeps the whole body, which says more than the list
	text     func(*verbCall, map[string]any)
	failed   func(map[string]any) bool // a 200 whose body reports failure
	// flags registers the verb's flags and returns what turns them into a request.
	flags func(*flag.FlagSet) func(*verbCall) error
	run   func(*verbCall) int // replaces the single request
	// client replaces the stored API key's client, for verbs that need a person.
	client func(*cmdCtx) (*Client, error)
}

// verbCall is one invocation; a prepare step may change anything on it.
type verbCall struct {
	apiVerb
	c    *cmdCtx
	cl   *Client
	pos  []string
	body any
}

// url fills the route's %s slots, so a slug passes through like an id: the
// control plane resolves it on every /v1/sandboxes/{id} route.
func (v *verbCall) url() string {
	var parts []any
	for _, p := range v.pos[:strings.Count(v.path, "%s")] {
		parts = append(parts, url.PathEscape(p))
	}
	return fmt.Sprintf(v.path, parts...)
}

// group makes the command-table entry for a set of verbs.
func group(name string, verbs []apiVerb, help, example string) command {
	var subs []string
	for _, v := range verbs {
		subs = append(subs, v.sub)
	}
	if len(subs) == 1 && subs[0] == "" {
		subs = nil
	}
	return command{
		name: name, subs: subs, help: help, example: example,
		run: func(c *cmdCtx, args []string) int { return runVerb(c, name, verbs, args) },
	}
}

// findVerb matches the longest sub, so "token create" beats a bare form taking
// "token" as an id.
func findVerb(verbs []apiVerb, args []string) (*apiVerb, []string) {
	match := func(sub string) *apiVerb {
		if sub == "ls" {
			sub = "list"
		}
		for i := range verbs {
			if verbs[i].sub == sub {
				return &verbs[i]
			}
		}
		return nil
	}
	if len(args) > 1 {
		if v := match(args[0] + " " + args[1]); v != nil {
			return v, args[2:]
		}
	}
	if len(args) > 0 {
		if v := match(args[0]); v != nil {
			return v, args[1:]
		}
	}
	return match(""), args
}

func verbUsage(group string, v apiVerb) string {
	return strings.Join(strings.Fields("usage: pilldrops "+group+" "+v.sub+" "+v.args), " ")
}

func runVerb(c *cmdCtx, group string, verbs []apiVerb, args []string) int {
	v, rest := findVerb(verbs, args)
	if v == nil {
		for _, known := range verbs {
			fmt.Fprintln(c.stderr, verbUsage(group, known))
		}
		return ExitUsage
	}
	name := strings.TrimSpace(group + " " + v.sub)
	confirmed := false
	if v.confirm {
		rest, confirmed = takeConfirm(rest)
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	var prepare func(*verbCall) error
	if v.flags != nil {
		prepare = v.flags(fs)
	}
	pos, err := parseMixed(fs, rest)
	if err != nil {
		fmt.Fprintln(c.stderr, verbUsage(group, *v))
		return ExitUsage
	}
	required := strings.Count(v.args, "<")
	if required > 0 && strings.HasPrefix(v.path, "/v1/sandboxes/") {
		pos = c.wantID(name, pos)
	}
	if len(pos) < required || len(pos) > required+v.optional {
		fmt.Fprintln(c.stderr, verbUsage(group, *v))
		return ExitUsage
	}
	if v.confirm && !confirmed {
		fmt.Fprintf(c.stderr, "pilldrops %s: this cannot be undone — re-run with --confirm\n", name)
		return ExitUsage
	}
	newClient := c.client
	if v.client != nil {
		newClient = func() (*Client, error) { return v.client(c) }
	}
	cl, err := newClient()
	if err != nil {
		return c.fail(err)
	}
	call := &verbCall{apiVerb: *v, c: c, cl: cl, pos: pos}
	if prepare != nil {
		if err := prepare(call); err != nil {
			fmt.Fprintf(c.stderr, "pilldrops %s: %v\n", name, err)
			return ExitUsage
		}
	}
	if call.run != nil {
		return call.run(call)
	}
	raw, err := call.send(call.method, call.url(), call.body)
	if err != nil {
		return c.fail(err)
	}
	return call.respond(raw)
}

// parseMixed lets flags and positionals come in either order; flag stops at the
// first positional, and these verbs read naturally with the id first. Everything
// after "--" is positional, so a message can start with a dash.
func parseMixed(fs *flag.FlagSet, args []string) ([]string, error) {
	var tail []string
	if i := slices.Index(args, "--"); i >= 0 {
		args, tail = args[:i], args[i+1:]
	}
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return append(pos, tail...), nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func (v *verbCall) send(method, path string, body any) ([]byte, error) {
	if body == nil {
		return v.cl.Raw(method, path, nil, "")
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return v.cl.Raw(method, path, payload, "application/json")
}

// respond renders a successful answer and decides the exit status.
func (v *verbCall) respond(raw []byte) int {
	c := v.c
	var obj map[string]any
	_ = json.Unmarshal(raw, &obj)
	switch {
	case c.jsonOut:
		v.printJSONBody(raw, obj)
	case v.text != nil:
		v.text(v, obj)
	case len(raw) == 0:
		if !c.quiet {
			fmt.Fprintln(c.stdout, "ok")
		}
	case c.quiet && v.method == "DELETE":
	case v.rows != "":
		rows := [][]string{}
		header := make([]string, len(v.cols))
		for i, col := range v.cols {
			header[i] = strings.ToUpper(col)
		}
		rows = append(rows, header)
		list, _ := obj[v.rows].([]any)
		for _, entry := range list {
			row, _ := entry.(map[string]any)
			cells := make([]string, len(v.cols))
			for i, col := range v.cols {
				cells[i] = renderValue(row[col])
			}
			rows = append(rows, cells)
		}
		table(c.stdout, rows)
	case obj != nil:
		printKV(c.stdout, obj)
	default:
		_, _ = c.stdout.Write(raw)
	}
	if v.failed != nil && v.failed(obj) {
		return ExitAPI
	}
	return ExitOK
}

// printJSONBody keeps --json at exactly one JSON value, whatever came back: an
// empty 204 and a null list included.
func (v *verbCall) printJSONBody(raw []byte, obj map[string]any) {
	switch {
	case len(raw) == 0:
		printJSON(v.c.stdout, map[string]string{"status": "ok"})
	case !json.Valid(raw):
		printJSON(v.c.stdout, map[string]string{"body": string(raw)})
	case v.rows != "" && !v.envelope:
		list, _ := obj[v.rows].([]any)
		if list == nil {
			list = []any{}
		}
		printJSON(v.c.stdout, list)
	default:
		printJSON(v.c.stdout, json.RawMessage(raw))
	}
}

// printField is the text rendering of a verb whose answer is one field: a
// string as it is, a list one entry per line.
func printField(key string) func(*verbCall, map[string]any) {
	return func(v *verbCall, obj map[string]any) {
		if list, ok := obj[key].([]any); ok {
			for _, entry := range list {
				fmt.Fprintln(v.c.stdout, renderValue(entry))
			}
			return
		}
		fmt.Fprintln(v.c.stdout, renderValue(obj[key]))
	}
}

// slow is for a request the server holds open for a whole agent turn or job.
func slow(cl *Client) {
	cl.HTTP = &http.Client{Timeout: 30 * time.Minute}
}

// envMap parses repeated KEY=VALUE flags into the map the API takes.
func envMap(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		k, val, ok := strings.Cut(pair, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--env %q is not KEY=VALUE", pair)
		}
		out[k] = val
	}
	return out, nil
}

// onOff reads a required pair of opposite flags into one bool.
func onOff(fs *flag.FlagSet, yes, no, usage string) func() (bool, error) {
	y := fs.Bool(yes, false, usage)
	n := fs.Bool(no, false, usage)
	return func() (bool, error) {
		if *y == *n {
			return false, fmt.Errorf("give exactly one of --%s and --%s", yes, no)
		}
		return *y, nil
	}
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

func withQuery(path, key string, n int) string {
	if n <= 0 {
		return path
	}
	return path + "?" + key + "=" + strconv.Itoa(n)
}

// statusNot reads a 200 whose status field says how the work really went.
func statusNot(want string) func(map[string]any) bool {
	return func(obj map[string]any) bool { return str(obj["status"]) != want }
}

// readDataFlag resolves `--data -` for the raw escape hatch.
func readDataFlag(data string) ([]byte, error) {
	if data == "-" {
		return io.ReadAll(stdin)
	}
	return []byte(data), nil
}

// readFileSpec reads `local[:remote]` into the API's {path, content}.
func readFileSpec(spec string) (map[string]string, error) {
	local, remote, named := strings.Cut(spec, ":")
	content, err := os.ReadFile(local)
	if err != nil {
		return nil, err
	}
	if !named {
		remote = local
	}
	// The API carries a file as a JSON string, which would mangle other bytes.
	if !utf8.Valid(content) {
		return nil, fmt.Errorf("%s is not UTF-8 text", local)
	}
	return map[string]string{"path": remote, "content": string(content)}, nil
}
