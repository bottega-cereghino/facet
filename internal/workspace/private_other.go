//go:build !darwin

package workspace

// privateBytes has nothing to ask outside APFS: every filesystem facet runs on
// elsewhere counts a file's blocks as its own, so the caller uses st_blocks.
func privateBytes(string) (int64, bool) { return 0, false }
