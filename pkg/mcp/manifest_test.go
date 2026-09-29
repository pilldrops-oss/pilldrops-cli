package mcp

import (
	"encoding/json"
	"flag"
	"os"
	"sort"
	"testing"
)

// manifestPath holds the tool names and input schemas this package declares.
// It is the parity contract with the platform, where these definitions are
// maintained (internal/mcp/tools.json there): the file is copied across
// verbatim, and each repo's own test fails the moment its catalog stops
// matching it. Descriptions are deliberately out: the task is to catch a tool
// or an argument appearing on one side only, not a reworded sentence.
//
// COPIED from the platform's internal/mcp/manifest_test.go, for the same
// reason platformtools.go is.
const manifestPath = "tools.json"

var updateManifest = flag.Bool("update", false, "rewrite tools.json from the catalog")

// manifest is {tool name: input schema}. encoding/json sorts map keys, so the
// rendering is deterministic without any sorting of our own.
func manifest() ([]byte, map[string]any) {
	m := map[string]any{}
	for _, t := range toolList() {
		m[t.name] = t.schema
	}
	raw, _ := json.MarshalIndent(m, "", "  ")
	return append(raw, '\n'), m
}

func TestToolManifestMatchesCatalog(t *testing.T) {
	want, live := manifest()

	if *updateManifest {
		if err := os.WriteFile(manifestPath, want, 0o644); err != nil {
			t.Fatalf("write %s: %v", manifestPath, err)
		}
		t.Logf("wrote %s with %d tools — this file belongs to the platform; only regenerate it "+
			"here after copying a platform change in", manifestPath, len(live))
		return
	}

	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read %s: %v\n  copy it from the platform's internal/mcp/tools.json", manifestPath, err)
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("parse %s: %v", manifestPath, err)
	}

	// Guard the check itself: an empty or tiny catalog would make every
	// comparison below vacuously pass.
	if len(live) < 30 {
		t.Fatalf("only %d tools in the catalog — toolList() has drifted from how tools are "+
			"declared, so this test is no longer checking anything", len(live))
	}

	var onlyHere, onlyThere, differ []string
	for name, schema := range live {
		s, ok := stored[name]
		if !ok {
			onlyHere = append(onlyHere, name)
			continue
		}
		if !sameJSON(schema, s) {
			differ = append(differ, name)
		}
	}
	for name := range stored {
		if _, ok := live[name]; !ok {
			onlyThere = append(onlyThere, name)
		}
	}
	sort.Strings(onlyHere)
	sort.Strings(onlyThere)
	sort.Strings(differ)

	for _, n := range onlyHere {
		t.Errorf("tool %q is in this CLI's catalog but not in %s — the platform does not have it", n, manifestPath)
	}
	for _, n := range onlyThere {
		t.Errorf("tool %q is in %s but not in this CLI's catalog — the platform has a tool this copy lost", n, manifestPath)
	}
	for _, n := range differ {
		t.Errorf("tool %q has a different input schema here than in %s", n, manifestPath)
	}
	if len(onlyHere)+len(onlyThere)+len(differ) > 0 {
		t.Logf("reconcile on the platform first, then copy internal/mcp/platformtools.go and " +
			"internal/mcp/tools.json over this package's copies")
	}
}

// sameJSON compares two values by their JSON rendering, so a schema built as
// map[string]any and one decoded from the file compare equal.
func sameJSON(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ab) == string(bb)
}
