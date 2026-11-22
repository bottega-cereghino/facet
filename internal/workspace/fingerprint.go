package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path/filepath"
)

// Fingerprint digests every entry under dir by its path, type, size and
// modification time, without following links. reap --force takes one when it
// shows the holders it will override and another just before it checks again:
// the holders' sentences ("app has uncommitted changes") read the same
// however much work lands while the prompt waits, but a new file, an edit, a
// commit, an amend or a stash writes something under dir and changes this. A
// tree that cannot be walked in full is an error.
func Fingerprint(dir string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(h, "%q %v %d %d\n", rel, fi.Mode(), fi.Size(), fi.ModTime().UnixNano())
		return err
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
