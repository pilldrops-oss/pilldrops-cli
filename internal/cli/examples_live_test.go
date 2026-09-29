//go:build live

// Every entry in the command table carries one worked example. This runs them
// all against a live control plane, so an example that rots into something that
// reads well and 404s fails the build instead of an agent's first tool call.
//
//	PILLDROPS_API_URL=https://plane PILLDROPS_API_KEY=sf_… \
//	  go test -tags live ./internal/cli -run TestCatalogExamplesRun -v
//
// Needs a fleet that can actually create a sandbox: the examples that name $SB
// run against one this test creates and destroys.
package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCatalogExamplesRun(t *testing.T) {
	base, key := os.Getenv("PILLDROPS_API_URL"), os.Getenv("PILLDROPS_API_KEY")
	if base == "" || key == "" {
		t.Fatal("set PILLDROPS_API_URL and PILLDROPS_API_KEY — this test is the live one, " +
			"and skipping it silently is how the examples rotted in the first place")
	}

	run := func(t *testing.T, args ...string) (int, string, string) {
		t.Helper()
		var out, errb strings.Builder
		code := Run(args, &out, &errb)
		return code, out.String(), errb.String()
	}

	// One sandbox for the whole sweep; $SB in an example resolves to it.
	code, out, errb := run(t, "--json", "sandbox", "create")
	if code != ExitOK {
		t.Fatalf("could not create the sandbox the examples run against: %s%s", out, errb)
	}
	sb := jsonField(t, out, "id")
	if sb == "" {
		t.Fatalf("create returned no id: %s", out)
	}
	t.Logf("examples run against %s", sb)
	defer run(t, "--json", "sandbox", "rm", sb)

	// A build context for the `image build` example.
	dockerDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dockerDir, "Dockerfile"), []byte("FROM alpine:3.20\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	subst := strings.NewReplacer(
		"$SB", sb,
		"$DOCKERDIR", dockerDir,
		"$PILLDROPS_API_KEY", key,
	)

	for _, c := range commandTable() {
		t.Run(c.name, func(t *testing.T) {
			args := strings.Fields(subst.Replace(c.example))
			if len(args) == 0 || args[0] != "pilldrops" {
				t.Fatalf("example is not a pilldrops invocation: %q", c.example)
			}
			code, out, errb := run(t, args[1:]...)
			if code != ExitOK {
				t.Errorf("example failed (exit %d): %s\n  stdout: %s\n  stderr: %s",
					code, c.example, out, errb)
			}
		})
	}
}

// jsonField pulls one top-level string field out of a --json body without
// pulling in a struct per command.
func jsonField(t *testing.T, body, key string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}
