package cli

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// ReleasesURL is where the latest release's binaries download from, the same
// files @pilldrops/cli fetches.
const ReleasesURL = "https://github.com/pilldrops-oss/pilldrops-cli/releases/latest/download"

// cmdUpgrade replaces the running binary with the latest published build.
// Download → write beside the executable → atomic rename, so a failed download
// never leaves a broken install. No-op (exit 0) when already current, detected
// by content hash — the binaries are reproducible enough for that (-trimpath).
func cmdUpgrade(c *cmdCtx, _ []string) int {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return c.fail(fmt.Errorf("upgrade: unsupported platform %s", runtime.GOOS))
	}
	arch := runtime.GOARCH
	if arch != "arm64" && arch != "amd64" {
		return c.fail(fmt.Errorf("upgrade: unsupported architecture %s", arch))
	}
	// The latest GitHub release, unless PILLDROPS_BASE_URL names a server that
	// serves the binaries itself under /dl/.
	url := fmt.Sprintf("%s/pilldrops_%s_%s", ReleasesURL, runtime.GOOS, arch)
	if base := os.Getenv("PILLDROPS_BASE_URL"); base != "" {
		url = fmt.Sprintf("%s/dl/pilldrops_%s_%s", base, runtime.GOOS, arch)
	}

	self, err := os.Executable()
	if err != nil {
		return c.fail(err)
	}
	self, _ = filepath.EvalSymlinks(self)

	if !c.quiet {
		fmt.Fprintf(c.stderr, "→ fetching %s …\n", url)
	}
	hc := &http.Client{Timeout: 120 * time.Second}
	resp, err := hc.Get(url)
	if err != nil {
		return c.fail(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return c.fail(fmt.Errorf("download failed (HTTP %d) from %s", resp.StatusCode, url))
	}

	tmp, err := os.CreateTemp(filepath.Dir(self), ".pilldrops-upgrade-*")
	if err != nil {
		return c.fail(fmt.Errorf("%v (need write access to %s)", err, filepath.Dir(self)))
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return c.fail(err)
	}
	tmp.Close()

	if same, err := sameFileHash(self, tmp.Name()); err == nil && same {
		fmt.Fprintf(c.stdout, "pilldrops %s is already the latest build\n", Version)
		return ExitOK
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return c.fail(err)
	}
	if err := os.Rename(tmp.Name(), self); err != nil {
		return c.fail(fmt.Errorf("%v (need write access to %s)", err, self))
	}
	fmt.Fprintf(c.stdout, "✓ upgraded %s — run `pilldrops version` to confirm\n", self)
	return ExitOK
}

func sameFileHash(a, b string) (bool, error) {
	ha, err := fileSHA256(a)
	if err != nil {
		return false, err
	}
	hb, err := fileSHA256(b)
	if err != nil {
		return false, err
	}
	return ha == hb, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
