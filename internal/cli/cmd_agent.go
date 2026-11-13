package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// agentPollEvery paces `agent ask --wait`; agentEventsIdle is how long a
// journal stays quiet before `agent events` without -f calls it read. The stream
// itself never ends, because the server holds it open for what comes next.
var (
	agentPollEvery  = 2 * time.Second
	agentEventsIdle = 2 * time.Second
)

const agentBase = "/v1/sandboxes/%s/agent"

func agentVerbs() []apiVerb {
	turnCols := []string{"id", "state", "mode", "thread", "request"}
	verbs := []apiVerb{
		{sub: "budget", args: "<id> [--cap USD]", method: "GET", path: agentBase + "/budget", flags: agentBudgetFlags},
		{sub: "ask", args: "<id> <message> [--mode supervised|guided|autonomous] [--thread T] [--priority] [--wait]",
			path: agentBase + "/turns", flags: agentAskFlags},
		{sub: "turns", args: "<id>", method: "GET", path: agentBase + "/turns", rows: "turns", cols: turnCols, envelope: true},
		{sub: "turn", args: "<id> <turn>", method: "GET", path: agentBase + "/turns/%s"},
		{sub: "interrupt", args: "<id> <turn> [--grace N]", method: "POST", path: agentBase + "/turns/%s/interrupt", flags: agentInterruptFlags},
		{sub: "dequeue", args: "<id> <turn> --confirm", method: "DELETE", path: agentBase + "/turns/%s", confirm: true},
		{sub: "events", args: "<id> [--since N] [-f]", path: agentBase + "/events", flags: agentEventsFlags},
		{sub: "threads", args: "<id>", method: "GET", path: agentBase + "/threads", rows: "threads",
			cols: []string{"id", "title", "turns", "last_at"}},
		{sub: "thread", args: "<id> <thread>", method: "GET", path: agentBase + "/threads/%s", rows: "turns", cols: turnCols, envelope: true},
		{sub: "tokens", args: "<id>", method: "GET", path: agentBase + "/tokens", rows: "tokens", envelope: true,
			cols: []string{"id", "label", "scopes", "active", "expires_at"}},
		{sub: "token create", args: "<id> --scope agent:read ... [--label L] [--ttl S]", method: "POST", path: agentBase + "/tokens", flags: agentTokenFlags},
		{sub: "token rm", args: "<id> <tokenID> --confirm", method: "DELETE", path: agentBase + "/tokens/%s", confirm: true},
		{sub: "token rotate", args: "<id> <tokenID>", method: "POST", path: agentBase + "/tokens/%s/rotate"},
		{sub: "share", args: "<id> [--scope agent:read ...]", method: "POST", path: agentBase + "/share", flags: agentShareFlags},
		{sub: "approve", args: "<id> <approvalID> --allow|--deny", method: "POST", path: agentBase + "/approvals/%s", flags: agentApproveFlags},
		{sub: "webhooks", args: "<id>", method: "GET", path: agentBase + "/webhooks", rows: "webhooks", envelope: true,
			cols: []string{"id", "url", "events", "enabled"}},
		{sub: "webhook add", args: "<id> <url> --event E ...", method: "POST", path: agentBase + "/webhooks", flags: webhookFlags(1)},
		{sub: "webhook rm", args: "<id> <hookID> --confirm", method: "DELETE", path: agentBase + "/webhooks/%s", confirm: true},
		{sub: "webhook test", args: "<id> <hookID>", method: "POST", path: agentBase + "/webhooks/%s/test",
			failed: func(obj map[string]any) bool { return obj["ok"] != true }},
	}
	// Reads with one id and nothing to render but the object itself.
	for _, sub := range []string{"status", "models", "connect", "approvals", "policy", "isolation"} {
		verbs = append(verbs, apiVerb{sub: sub, args: "<id>", method: "GET", path: agentBase + "/" + sub})
	}
	return verbs
}

func agentBudgetFlags(fs *flag.FlagSet) func(*verbCall) error {
	capUSD := fs.Float64("cap", 0, "daily spend cap in USD; 0 clears it")
	return func(v *verbCall) error {
		if flagWasSet(fs, "cap") {
			v.method, v.body = "PATCH", map[string]any{"cap_usd": *capUSD}
		}
		return nil
	}
}

func agentInterruptFlags(fs *flag.FlagSet) func(*verbCall) error {
	grace := fs.Float64("grace", 0, "seconds the agent may stop politely before being killed")
	return func(v *verbCall) error {
		if flagWasSet(fs, "grace") {
			v.body = map[string]any{"grace_seconds": *grace}
		}
		return nil
	}
}

func scopedBody(scopes []string, label string, ttl int) map[string]any {
	body := map[string]any{}
	if len(scopes) > 0 {
		body["scopes"] = scopes
	}
	if label != "" {
		body["label"] = label
	}
	if ttl != 0 {
		body["ttl_seconds"] = ttl
	}
	return body
}

func agentTokenFlags(fs *flag.FlagSet) func(*verbCall) error {
	var scopes stringList
	fs.Var(&scopes, "scope", "scope to grant, repeatable (agent:read, agent:write, ...)")
	label := fs.String("label", "", "shown in the token list")
	ttl := fs.Int("ttl", 0, "lifetime in seconds; the server defaults and clamps it")
	return func(v *verbCall) error {
		if len(scopes) == 0 {
			return errors.New("name at least one --scope")
		}
		v.body = scopedBody(scopes, *label, *ttl)
		return nil
	}
}

func agentShareFlags(fs *flag.FlagSet) func(*verbCall) error {
	var scopes stringList
	fs.Var(&scopes, "scope", "scope the link carries, repeatable; default agent:read")
	return func(v *verbCall) error {
		v.body = scopedBody(scopes, "", 0)
		return nil
	}
}

func agentApproveFlags(fs *flag.FlagSet) func(*verbCall) error {
	allowed := onOff(fs, "allow", "deny", "the decision")
	return func(v *verbCall) error {
		allow, err := allowed()
		if err != nil {
			return err
		}
		decision := "deny"
		if allow {
			decision = "allow"
		}
		v.body = map[string]any{"decision": decision}
		return nil
	}
}

func webhookFlags(urlPos int) func(*flag.FlagSet) func(*verbCall) error {
	return func(fs *flag.FlagSet) func(*verbCall) error {
		var events stringList
		fs.Var(&events, "event", "event type to deliver, repeatable")
		return func(v *verbCall) error {
			if len(events) == 0 {
				return errors.New("name at least one --event")
			}
			v.body = map[string]any{"url": v.pos[urlPos], "events": []string(events)}
			return nil
		}
	}
}

func agentAskFlags(fs *flag.FlagSet) func(*verbCall) error {
	mode := fs.String("mode", "", "supervised, guided or autonomous")
	thread := fs.String("thread", "", "latest (default), new, or a thread id")
	priority := fs.Bool("priority", false, "jump the queue")
	wait := fs.Bool("wait", false, "wait for the turn to finish and exit 1 if it did not succeed")
	return func(v *verbCall) error {
		switch *mode {
		case "", "supervised", "guided", "autonomous":
		default:
			return fmt.Errorf("--mode %q is not supervised, guided or autonomous", *mode)
		}
		body := map[string]any{"message": v.pos[1]}
		if *mode != "" {
			body["mode"] = *mode
		}
		if *thread != "" {
			body["thread"] = *thread
		}
		if *priority {
			body["priority"] = true
		}
		v.method, v.body = "POST", body
		// Submitting may first have to resume a paused workstation.
		slow(v.cl)
		v.run = func(v *verbCall) int {
			raw, err := v.send(v.method, v.url(), v.body)
			if err != nil {
				return v.c.fail(err)
			}
			if !*wait {
				return v.respond(raw)
			}
			var turn struct {
				ID string `json:"turn_id"`
			}
			if err := json.Unmarshal(raw, &turn); err != nil || turn.ID == "" {
				return v.c.fail(fmt.Errorf("the server accepted the turn but named none: %s", summarise(string(raw))))
			}
			return v.waitForTurn(turn.ID)
		}
		return nil
	}
}

// waitForTurn polls until the turn ends. A turn parked on an approval is not
// finished, so that wait is as long as the approver takes.
func (v *verbCall) waitForTurn(id string) int {
	c, path := v.c, v.url()+"/"+url.PathEscape(id)
	failures, noted := 0, false
	for {
		raw, err := v.send("GET", path, nil)
		if err != nil {
			var apiErr *APIError
			refused := errors.As(err, &apiErr) && apiErr.Status < 500 && apiErr.Status != 429
			if failures++; refused || failures >= 3 {
				return c.fail(err)
			}
			time.Sleep(agentPollEvery)
			continue
		}
		failures = 0
		var turn struct {
			State    string `json:"state"`
			Response string `json:"response"`
			Error    string `json:"error"`
		}
		if err := json.Unmarshal(raw, &turn); err != nil {
			return c.fail(fmt.Errorf("unreadable turn: %w", err))
		}
		switch turn.State {
		case "done", "failed", "interrupted":
			if c.jsonOut {
				printJSON(c.stdout, json.RawMessage(raw))
			} else if turn.Response != "" {
				fmt.Fprintln(c.stdout, turn.Response)
			}
			if turn.State == "done" {
				return ExitOK
			}
			if !c.jsonOut {
				fmt.Fprintf(c.stderr, "pilldrops: turn %s %s: %s\n", id, turn.State, turn.Error)
			}
			return ExitAPI
		case "awaiting_approval":
			if !noted && !c.jsonOut && !c.quiet {
				fmt.Fprintf(c.stderr, "turn %s is waiting on an approval: pilldrops agent approvals %s\n", id, v.pos[0])
				noted = true
			}
		}
		time.Sleep(agentPollEvery)
	}
}

func agentEventsFlags(fs *flag.FlagSet) func(*verbCall) error {
	since := fs.Uint64("since", 0, "only events after this sequence number")
	follow := fs.Bool("f", false, "keep following (text output only)")
	fs.BoolVar(follow, "follow", false, "keep following (text output only)")
	return func(v *verbCall) error {
		if *follow && v.c.jsonOut {
			return errors.New("-f never ends, so it is not one JSON value; poll with --since instead")
		}
		v.path += "?since_seq=" + strconv.FormatUint(*since, 10)
		v.run = func(v *verbCall) int { return v.streamEvents(*follow) }
		return nil
	}
}

// streamEvents prints each journal event. Without follow it stops once the
// journal has gone quiet, and --json collects what it read into one array.
func (v *verbCall) streamEvents(follow bool) int {
	c := v.c
	body, err := v.cl.Stream(v.url())
	if err != nil {
		return c.fail(err)
	}
	defer body.Close()

	lines := make(chan string)
	scanErr := make(chan error, 1)
	go func() {
		defer close(lines)
		in := bufio.NewScanner(body)
		in.Buffer(make([]byte, 1<<20), 16<<20) // one event can carry a whole agent answer
		for in.Scan() {
			if data, ok := strings.CutPrefix(in.Text(), "data:"); ok {
				lines <- strings.TrimSpace(data)
			}
		}
		scanErr <- in.Err()
	}()

	events := []json.RawMessage{}
	var quiet <-chan time.Time
	if !follow {
		quiet = time.After(agentEventsIdle)
	}
	for {
		select {
		case data, open := <-lines:
			if !open {
				if err := <-scanErr; err != nil && follow {
					return c.fail(fmt.Errorf("the event stream was lost: %w", err))
				}
				if c.jsonOut {
					printJSON(c.stdout, events)
				}
				return ExitOK
			}
			if c.jsonOut && json.Valid([]byte(data)) {
				events = append(events, json.RawMessage(data))
			} else if !c.jsonOut {
				fmt.Fprintln(c.stdout, data)
			}
			if !follow {
				quiet = time.After(agentEventsIdle)
			}
		case <-quiet:
			if c.jsonOut {
				printJSON(c.stdout, events)
			}
			return ExitOK
		}
	}
}
