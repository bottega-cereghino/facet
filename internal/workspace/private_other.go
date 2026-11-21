//go:build !darwin

package workspace

// privateBytes has nothing to ask outside APFS, so the caller uses st_blocks.
// That overstates on a filesystem that shares extents between reflinked copies
// (btrfs, XFS): the figure there is an upper bound.
func privateBytes(string) (int64, bool) { return 0, false }
