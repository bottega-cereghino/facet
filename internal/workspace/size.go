package workspace

import (
	"io/fs"
	"path/filepath"
	"syscall"
	"time"
)

// freedBytes is how much disk deleting root gives back: each file's allocated
// blocks, counted once per inode, and only when every name the inode has is
// inside root. A clone made from a mirror shares its objects with it through
// hardlinks, so they outlive the workspace and are not counted; a symlink
// counts as itself, never as its target. A tree that cannot be read counts
// what could be.
//
// On APFS a file counts only the extents no clone of it shares (privateBytes),
// so a node_modules cloned from a package store is not counted as a copy. Two
// clones of one file that both sit inside root count nothing for it, which
// makes the figure a lower bound there.
func freedBytes(root string) int64 {
	n, _ := treeStats(root)
	return n
}

// treeStats is freedBytes, and the newest modification time of any entry in
// the tree, from the same walk. pip installs into .venv/lib/.../site-packages
// and a patch-package edit lands deep in node_modules: neither touches the top
// directory, so only the newest entry dates the last use. Directories count
// as well as files, since npm extracts files with a fixed old mtime and only
// the directories it creates carry the time of the install.
func treeStats(root string) (total int64, newest time.Time) {
	type inode struct{ dev, ino uint64 }
	seen := map[inode]int{}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			total += fi.Size()
			return nil
		}
		blocks := int64(st.Blocks) * 512
		if d.Type().IsRegular() {
			if p, ok := privateBytes(path); ok && p < blocks {
				blocks = p
			}
		}
		if d.IsDir() || st.Nlink <= 1 {
			total += blocks
			return nil
		}
		// A file with several names: count it when the last of them turns up
		// inside root, which never happens if one of them lives outside.
		key := inode{uint64(st.Dev), uint64(st.Ino)} //nolint:unconvert // Dev and Ino differ in type across platforms
		seen[key]++
		if uint64(seen[key]) == uint64(st.Nlink) { //nolint:unconvert // Nlink differs in type across platforms
			total += blocks
		}
		return nil
	})
	return total, newest
}
