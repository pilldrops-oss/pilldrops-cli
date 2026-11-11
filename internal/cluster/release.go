package cluster

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const releaseRepo = "pilldrops-oss/pilldrops"

// releasePub verifies releases signed by scripts/release-local.sh. It is a
// copy of release.pub in the engine repo, taken from here rather than from the
// release, which an attacker who swapped the binaries could also swap.
//
//go:embed release.pub
var releasePub []byte

// releaseFiles are the assets apply installs; each must be in SHA256SUMS.
var releaseFiles = []string{
	"orchestrator-linux-amd64",
	"sandboxd-fleet-linux-amd64",
	"envd-linux-amd64",
	"pilldrops-join-linux-amd64",
	"pilldrops-linux-amd64",
	"source.tar.gz",
}

var releaseBase = "https://github.com/" + releaseRepo + "/releases/download/"

// fetchSums downloads the release's SHA256SUMS and checks its signature with
// cosign. The hosts then download each binary themselves and check it against
// these sums, so the operator's machine never carries the binaries.
func fetchSums(tag string) ([]byte, error) {
	if _, err := exec.LookPath("cosign"); err != nil {
		return nil, fmt.Errorf("cosign is not installed; apply checks the release signature with it (https://docs.sigstore.dev/cosign/installation/)")
	}
	dir, err := os.MkdirTemp("", "pilldrops-release-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	base := releaseBase + tag + "/"
	sums, err := download(base + "SHA256SUMS")
	if err != nil {
		return nil, err
	}
	sig, err := download(base + "SHA256SUMS.sig")
	if err != nil {
		return nil, err
	}
	pem, pemErr := download(base + "SHA256SUMS.pem")
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err == nil {
			err = os.WriteFile(p, b, 0o600)
		}
		return p
	}
	sumsPath, sigPath := write("SHA256SUMS", sums), write("SHA256SUMS.sig", sig)
	var args []string
	if pemErr == nil {
		// Signed by the release workflow: pin the identity, or any Sigstore
		// identity would pass.
		args = []string{"verify-blob", "--certificate", write("SHA256SUMS.pem", pem), "--signature", sigPath,
			"--certificate-identity-regexp", `^https://github.com/` + releaseRepo + `/\.github/workflows/release\.yml@refs/tags/` + regexp.QuoteMeta(tag) + `$`,
			"--certificate-oidc-issuer", "https://token.actions.githubusercontent.com", sumsPath}
	} else {
		args = []string{"verify-blob", "--insecure-ignore-tlog=true", "--key", write("release.pub", releasePub),
			"--signature", sigPath, sumsPath}
	}
	if err != nil {
		return nil, err
	}
	if out, err := exec.Command("cosign", args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("the signature over %s SHA256SUMS does not verify; nothing was installed:\n%s", tag, out)
	}
	if missing := missingFiles(sums); len(missing) > 0 {
		return nil, fmt.Errorf("release %s does not ship %s; use sandboxd/v1.2.1 or later", tag, strings.Join(missing, ", "))
	}
	return sums, nil
}

func missingFiles(sums []byte) []string {
	listed := map[string]bool{}
	for _, line := range strings.Split(string(sums), "\n") {
		if f := strings.Fields(line); len(f) == 2 {
			listed[strings.TrimPrefix(f[1], "*")] = true
		}
	}
	var missing []string
	for _, f := range releaseFiles {
		if !listed[f] {
			missing = append(missing, f)
		}
	}
	return missing
}

func download(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	var b bytes.Buffer
	_, err = io.Copy(&b, io.LimitReader(resp.Body, 1<<20))
	return b.Bytes(), err
}
