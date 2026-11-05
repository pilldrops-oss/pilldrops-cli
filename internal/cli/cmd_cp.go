package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// stdin is the upload source for `cp - <id>:<path>`; a var so tests can feed it.
var stdin io.Reader = os.Stdin

// tarDir builds a gzipped tar of dir with the directory itself as the
// top-level entry (scp semantics: `cp -r proj id:.` lands at ~/proj).
func tarDir(dir string) ([]byte, error) {
	return tarTree(dir, filepath.Base(filepath.Clean(dir)))
}

// tarContext builds a docker build context: dir's own contents at the root, so
// its Dockerfile is at "Dockerfile". Unlike cp's tar it keeps symlink targets,
// because docker refuses to unpack a link that has none.
func tarContext(dir string) ([]byte, error) {
	return tarTree(dir, "")
}

// tarTree tars dir with every entry under prefix.
func tarTree(dir, prefix string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = path.Join(prefix, filepath.ToSlash(rel))
		if hdr.Name == "." {
			return nil
		}
		if prefix == "" && fi.Mode()&os.ModeSymlink != 0 {
			if hdr.Linkname, err = os.Readlink(p); err != nil {
				return err
			}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := io.Copy(tw, f); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// sandboxPath parses "<id>:<path>" (id may not contain '/' or '.', so a bare
// local filename like "a.txt" or "./x" is never mistaken for a remote spec).
func sandboxPath(s string) (id, path string, ok bool) {
	i := strings.Index(s, ":")
	if i <= 0 || strings.ContainsAny(s[:i], "/.") {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

// homePath maps the sandbox side onto the files API, where a relative path is
// the home directory on both runtimes: "~/x" → "x", "~" and "" → ".".
// An absolute path stays absolute; it names a place in the guest.
func homePath(p string) string {
	switch {
	case p == "" || p == "~":
		return "."
	case strings.HasPrefix(p, "~/"):
		return strings.TrimPrefix(p, "~/")
	}
	return p
}

// intoDir is cp's own rule: a destination that names a directory takes the file
// under its own name. Only the spellings that say so without asking the guest —
// a trailing slash, and the home directory — because a bare "/tmp" cannot be
// told from a file called tmp without a round trip.
//
// Without it `cp f.txt sb:/tmp/` sent "/tmp/" to the files API as the name of a
// file to write, and the sandbox refused it as an invalid path.
func intoDir(dst, name string) string {
	if dst == "." || dst == "" {
		return name
	}
	if strings.HasSuffix(dst, "/") {
		return dst + name
	}
	return dst
}

const cpUsage = "usage: pilldrops cp [-r] <local>|- <id>:<path>  |  pilldrops cp [-r] <id>:<path> <local>|-"

func cmdCp(c *cmdCtx, args []string) int {
	fs := flag.NewFlagSet("cp", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	recursive := fs.Bool("r", false, "copy a directory")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(c.stderr, cpUsage)
		return ExitUsage
	}
	args = fs.Args()
	if len(args) == 1 {
		if other, ok := c.askCpOther(args[0]); ok {
			args = append(args, other)
		}
	}
	if len(args) != 2 {
		fmt.Fprintln(c.stderr, cpUsage)
		return ExitUsage
	}
	srcID, srcPath, srcRemote := sandboxPath(args[0])
	dstID, dstPath, dstRemote := sandboxPath(args[1])
	if srcRemote == dstRemote { // both or neither remote
		fmt.Fprintln(c.stderr, "pilldrops cp: exactly one side must be <id>:<path>")
		return ExitUsage
	}
	cl, err := c.client()
	if err != nil {
		return c.fail(err)
	}

	if dstRemote { // upload
		dstPath = homePath(dstPath)
		var data []byte
		if args[0] == "-" {
			if data, err = io.ReadAll(stdin); err != nil {
				return c.fail(err)
			}
		} else {
			fi, err := os.Stat(args[0])
			if err != nil {
				return c.fail(err)
			}
			if fi.IsDir() {
				if !*recursive {
					fmt.Fprintf(c.stderr, "pilldrops cp: %s is a directory — use -r to copy a directory\n", args[0])
					return ExitUsage
				}
				return c.cpUploadDir(cl, args[0], dstID, dstPath)
			}
			if data, err = os.ReadFile(args[0]); err != nil {
				return c.fail(err)
			}
			dstPath = intoDir(dstPath, filepath.Base(args[0]))
		}
		if err := cpUpload(cl, dstID, dstPath, data); err != nil {
			return c.fail(err)
		}
		if c.jsonOut {
			printJSON(c.stdout, map[string]any{"uploaded": dstPath, "bytes": len(data)})
		} else if !c.quiet {
			fmt.Fprintf(c.stdout, "uploaded %s (%d bytes)\n", dstPath, len(data))
		}
		return ExitOK
	}

	// download — directory-aware: probe the remote, and if it is a directory,
	// tar it server-side, pull the archive, and unpack it locally with the
	// destination as the parent (e.g. a Playwright HTML report directory).
	srcRel := homePath(srcPath)
	var probe map[string]any
	_ = cl.JSON("POST", "/v1/sandboxes/"+srcID+"/exec",
		map[string]any{"cmd": fmt.Sprintf("test -d %q && echo DIR || echo FILE", srcRel), "timeout": 15}, &probe)
	if out, _ := probe["stdout"].(string); strings.HasPrefix(out, "DIR") {
		if !*recursive {
			fmt.Fprintf(c.stderr, "pilldrops cp: %s is a directory — use -r to copy a directory\n", args[0])
			return ExitUsage
		}
		if args[1] == "-" {
			fmt.Fprintln(c.stderr, "pilldrops cp: a directory cannot be written to stdout")
			return ExitUsage
		}
		tmp := fmt.Sprintf("_sf_dl_%d.tgz", time.Now().UnixNano())
		var tres map[string]any
		if err := cl.JSON("POST", "/v1/sandboxes/"+srcID+"/exec",
			map[string]any{"cmd": fmt.Sprintf("tar -czf %q -C %q %q", tmp, path.Dir(srcRel), path.Base(srcRel)), "timeout": 120}, &tres); err != nil {
			return c.fail(err)
		}
		if code, _ := tres["exit_code"].(float64); code != 0 {
			return c.fail(fmt.Errorf("remote tar failed: %v", tres["stderr"]))
		}
		archive, err := cl.Raw("GET", "/v1/sandboxes/"+srcID+"/files?path="+url.QueryEscape(tmp), nil, "")
		if err != nil {
			return c.fail(err)
		}
		_ = cl.JSON("POST", "/v1/sandboxes/"+srcID+"/exec",
			map[string]any{"cmd": "rm -f " + tmp, "timeout": 10}, &map[string]any{})
		if err := untarInto(archive, args[1]); err != nil {
			return c.fail(err)
		}
		if c.jsonOut {
			printJSON(c.stdout, map[string]any{"downloaded_dir": args[1], "bytes": len(archive)})
		} else if !c.quiet {
			fmt.Fprintf(c.stdout, "downloaded %s/ (%d bytes, unpacked)\n", args[1], len(archive))
		}
		return ExitOK
	}

	p := "/v1/sandboxes/" + srcID + "/files?path=" + url.QueryEscape(srcRel)
	data, err := cl.Raw("GET", p, nil, "")
	if err != nil {
		return c.fail(err)
	}
	if args[1] == "-" {
		_, err := c.stdout.Write(data)
		return exitFor(c, err)
	}
	if err := os.WriteFile(args[1], data, 0o644); err != nil {
		return c.fail(err)
	}
	if c.jsonOut {
		printJSON(c.stdout, map[string]any{"downloaded": args[1], "bytes": len(data)})
	} else if !c.quiet {
		fmt.Fprintf(c.stdout, "downloaded %s (%d bytes)\n", args[1], len(data))
	}
	return ExitOK
}

func exitFor(c *cmdCtx, err error) int {
	if err != nil {
		return c.fail(err)
	}
	return ExitOK
}

// cpUpload POSTs data to the files API and checks the byte count the server
// reports against what was sent; a short write is an error naming both.
func cpUpload(cl *Client, id, remote string, data []byte) error {
	resp, err := cl.Raw("POST", "/v1/sandboxes/"+id+"/files?path="+url.QueryEscape(remote), data, "application/octet-stream")
	if err != nil {
		return err
	}
	var got struct {
		Bytes int64 `json:"bytes"`
	}
	if err := json.Unmarshal(resp, &got); err != nil {
		return fmt.Errorf("files API returned no byte count: %s", resp)
	}
	if got.Bytes != int64(len(data)) {
		return fmt.Errorf("short write: sent %d bytes, sandbox stored %d", len(data), got.Bytes)
	}
	return nil
}

// cpUploadDir tars dir (as its own top-level entry), uploads the archive and
// untars it under the remote parent — one round-trip for a whole project.
// A relative parent is home-relative, so exec's cwd and the /files root agree;
// an absolute one is left alone. It used to have its leading slash stripped,
// which quietly turned `cp -r ./x sb:/tmp/dest` into a write to ~/tmp/dest and
// still reported success.
//
// The archive lands INSIDE the destination, not in the home directory, and that
// placement is the whole confinement story. `cp f sb:/etc/marker` was refused —
// "file paths are taken relative to the sandbox home, and one that resolves
// outside it is refused" — while `cp -r d sb:/etc` wrote there and reported
// "unpacked", because the recursive path never showed the destination to the
// files API: it uploaded to the home directory and untarred through /exec,
// which confines nothing. One stated boundary enforced on one of two paths is
// worse than none, because a test that checks it with a single file passes.
//
// Writing the archive to <parent>/<tmp> puts the destination back in front of
// the authority that owns the rule, so the two spellings of cp agree by
// construction: whatever the files API refuses for one file it refuses for a
// directory, in the same words, and nothing is restated or guessed here. The
// write creates the parent (envd WriteFile and workdirFS.Write both MkdirAll),
// so the mkdir -p this used to run first — which created /etc/evil before
// anything was refused — goes with it.
func (c *cmdCtx) cpUploadDir(cl *Client, dir, id, parent string) int {
	if parent == "" {
		parent = "."
	}
	data, err := tarDir(dir)
	if err != nil {
		return c.fail(err)
	}
	tmp := path.Join(parent, fmt.Sprintf("_sf_up_%d.tgz", time.Now().UnixNano()))
	if err := cpUpload(cl, id, tmp, data); err != nil {
		return c.fail(err)
	}
	exec := map[string]any{
		"cmd":     fmt.Sprintf("tar -xzf %q -C %q && rm -f %q", tmp, parent, tmp),
		"timeout": 60,
	}
	var res map[string]any
	if err := cl.JSON("POST", "/v1/sandboxes/"+id+"/exec", exec, &res); err != nil {
		return c.fail(err)
	}
	if code, _ := res["exit_code"].(float64); code != 0 {
		return c.fail(fmt.Errorf("untar failed: %v", res["stderr"]))
	}
	remote := path.Join(parent, filepath.Base(filepath.Clean(dir)))
	if c.jsonOut {
		printJSON(c.stdout, map[string]any{"uploaded_dir": remote, "bytes": len(data)})
	} else if !c.quiet {
		fmt.Fprintf(c.stdout, "uploaded %s/ (%d bytes, unpacked)\n", remote, len(data))
	}
	return ExitOK
}

// untarInto extracts a gzipped tar into dir (created if needed), guarding against
// path traversal in archive entries.
func untarInto(archive []byte, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		clean := filepath.Clean(hdr.Name)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			continue // skip anything trying to escape dir
		}
		target := filepath.Join(dir, clean)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		default:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
}
