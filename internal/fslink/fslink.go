// Package fslink creates and inspects the symlinks that back a workspace's
// `links` entries.
//
// The safety property that matters: Remove deletes only the link itself, never
// the target's contents, and refuses outright when handed something that is not
// a link.
package fslink

import (
	"fmt"
	"os"
	"path/filepath"
)

// ErrNotALink is returned by Remove when the path exists but is not a link.
type ErrNotALink struct{ Path string }

func (e *ErrNotALink) Error() string {
	return fmt.Sprintf("%s is not a link; refusing to delete", e.Path)
}

// Remove deletes the link at path, never the directory it points at. It refuses
// if path exists and is not a link. A missing path is not an error.
func Remove(path string) error {
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	ok, err := IsLink(path)
	if err != nil {
		return err
	}
	if !ok {
		return &ErrNotALink{Path: path}
	}
	return os.Remove(path)
}

// Create makes link a symlink pointing at target.
func Create(link, target string) error {
	target, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	return os.Symlink(target, link)
}

// IsLink reports whether path is a symlink.
func IsLink(path string) (bool, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return fi.Mode()&os.ModeSymlink != 0, nil
}

// Read returns the symlink's target. ok is false when path is not a symlink.
func Read(path string) (target string, ok bool, err error) {
	isLink, err := IsLink(path)
	if err != nil || !isLink {
		return "", false, nil
	}
	target, err = os.Readlink(path)
	if err != nil {
		return "", false, err
	}
	return target, true, nil
}
