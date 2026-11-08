package cli

import (
	"fmt"
	"net/http"
	"slices"
	"time"
)

// controlPlaneFeature is what a server with a control plane reports; a single
// node answers neither these routes nor any other optional group.
const controlPlaneFeature = "control-plane"

// supports says whether this server serves an optional feature group. Not
// knowing counts as yes, whether logged out, unreachable or a server too old to
// report: the command then runs and reports its own error, as it always did.
func (c *cmdCtx) supports(feature string) bool {
	if !c.featuresLoaded {
		c.featuresLoaded = true
		c.loadFeatures()
	}
	return c.features == nil || slices.Contains(c.features, feature)
}

func (c *cmdCtx) loadFeatures() {
	cl, err := c.client()
	if err != nil {
		return
	}
	cl.HTTP = &http.Client{Timeout: 3 * time.Second}
	var caps struct {
		Features []string `json:"features"`
	}
	if err := cl.JSON("GET", "/v1/capabilities", nil, &caps); err == nil {
		c.features = caps.Features
	}
}

// requireFeature is the refusal for a group this server lacks; without it the
// caller gets a bare 404 and a hint about sandbox ids.
func (c *cmdCtx) requireFeature(feature string) error {
	if c.supports(feature) {
		return nil
	}
	hint := "it is available on PillDrops Cloud; `pilldrops capabilities` lists what this server supports"
	if feature == controlPlaneFeature {
		hint = "this server is a single node; these commands need a control plane, see docs/deployment.md"
	}
	return pathError{
		err: &APIError{
			Status: http.StatusNotImplemented,
			Code:   "feature_unavailable",
			Msg:    fmt.Sprintf("this server does not support %s", feature),
		},
		hint: hint,
	}
}

// unsupportedFeatures lists the table's groups this server lacks, for saying
// what help left out.
func (c *cmdCtx) unsupportedFeatures() []string {
	missing := []string{}
	for _, cmd := range commandTable() {
		if cmd.feature != "" && !c.supports(cmd.feature) && !slices.Contains(missing, cmd.feature) {
			missing = append(missing, cmd.feature)
		}
	}
	return missing
}
