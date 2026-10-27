package ccds

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	vaultBackupKeep = 30
	vaultMaxBytes   = 5 << 20
)

// vault stores only the ciphertext the browser extension produces; the
// master password and plaintext never reach this process.
type vault struct {
	dir   string
	token string
	mu    sync.Mutex
}

type vaultRecord struct {
	Rev       int64           `json:"rev"`
	UpdatedAt int64           `json:"updatedAt,omitempty"`
	Blob      json.RawMessage `json:"blob"`
}

func (v *vault) file() string      { return filepath.Join(v.dir, "vault.json") }
func (v *vault) tokenFile() string { return filepath.Join(v.dir, "vault-token") }
func (v *vault) backupDir() string { return filepath.Join(v.dir, "backups") }

// loadToken reads the token the extension is configured with, creating one on first run.
func (v *vault) loadToken() error {
	if err := os.MkdirAll(v.dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(v.dir, 0o700); err != nil {
		return err
	}
	data, err := os.ReadFile(v.tokenFile())
	if err == nil {
		if t := strings.TrimSpace(string(data)); t != "" {
			v.token = t
			return nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	if err := os.WriteFile(v.tokenFile(), []byte(token+"\n"), 0o600); err != nil {
		return err
	}
	v.token = token
	return nil
}

func (v *vault) read() (vaultRecord, error) {
	data, err := os.ReadFile(v.file())
	if errors.Is(err, fs.ErrNotExist) {
		return vaultRecord{}, nil
	}
	if err != nil {
		return vaultRecord{}, err
	}
	var rec vaultRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return vaultRecord{}, fmt.Errorf("parse %s: %w", v.file(), err)
	}
	return rec, nil
}

// write backs up the previous version before an atomic replace, so a vault
// overwritten by an extension bug or mistake can be recovered from backups/.
func (v *vault) write(rec vaultRecord) error {
	if _, err := os.Stat(v.file()); err == nil {
		if err := os.MkdirAll(v.backupDir(), 0o700); err != nil {
			return err
		}
		dst := filepath.Join(v.backupDir(), fmt.Sprintf("vault-%06d.json", rec.Rev-1))
		if err := copyFile(v.file(), dst); err != nil {
			return err
		}
		entries, err := os.ReadDir(v.backupDir())
		if err != nil {
			return err
		}
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		sort.Strings(names)
		for _, old := range names[:max(len(names)-vaultBackupKeep, 0)] {
			os.Remove(filepath.Join(v.backupDir(), old))
		}
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp := v.file() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, v.file())
}

// copyFile keeps the source's mode and mtime.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
		return err
	}
	return os.Chtimes(dst, info.ModTime(), info.ModTime())
}
