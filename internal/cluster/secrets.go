package cluster

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
)

// secretNames are generated once and then reused on every apply. Each is 32
// random bytes in hex, which every consumer accepts and which needs no quoting
// in shell, SQL or a DSN.
var secretNames = []string{
	"ORCHESTRATOR_API_KEY", // the operator key; apply uses it for the API
	"ORCHESTRATOR_TOKEN",
	"NODE_KEY_SECRET", // losing it means joining every node again
	"ORG_ASSERT_KEY",
	"VAULT_SECRET_KEY",
	"SANDBOXD_SECRET_KEY", // also handed to nodes as NODE_BOOTSTRAP_SECRET
	"POSTGRES_PASSWORD",
	"NODE_DB_PASSWORD",
}

// Secrets is cluster.secrets: KEY=VALUE lines, mode 0600.
type Secrets map[string]string

// LoadSecrets reads path, adds any secret it lacks, and writes it back. A file
// others can read is refused rather than fixed, since the damage is done.
func LoadSecrets(path string) (Secrets, bool, error) {
	s := Secrets{}
	f, err := os.Open(path)
	switch {
	case err == nil:
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return nil, false, err
		}
		if fi.Mode().Perm()&0o077 != 0 {
			return nil, false, fmt.Errorf("%s can be read by others (mode %v); rotate every secret in it, then chmod 600", path, fi.Mode().Perm())
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "="); ok && !strings.HasPrefix(k, "#") {
				s[k] = v
			}
		}
		if err := sc.Err(); err != nil {
			return nil, false, err
		}
	case os.IsNotExist(err):
	default:
		return nil, false, err
	}
	added := false
	for _, name := range secretNames {
		if s[name] != "" {
			continue
		}
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return nil, false, err
		}
		s[name] = hex.EncodeToString(b)
		added = true
	}
	if added {
		if err := s.write(path); err != nil {
			return nil, false, err
		}
	}
	return s, added, nil
}

func (s Secrets) write(path string) error {
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# Written by `pilldrops cluster apply`. Keep it, back it up, never commit it.\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, s[k])
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
