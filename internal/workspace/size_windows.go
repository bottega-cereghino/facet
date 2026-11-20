package workspace

import (
	"io/fs"
	"path/filepath"
)

// freedBytes adds up apparent file sizes. Windows is unsupported; this keeps
// the package building there without claiming the Unix accounting.
func freedBytes(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			total += fi.Size()
		}
		return nil
	})
	return total
}
