package launchd

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/unix"
)

// volCapFmtCaseSensitive is VOL_CAP_FMT_CASE_SENSITIVE from <sys/attr.h>.
// x/sys exports ATTR_VOL_CAPABILITIES but not the capability bits.
const volCapFmtCaseSensitive = 0x00000100

// foldsCase reports whether path's volume compares names without regard to
// case. A volume that cannot say is treated as case-sensitive: folding a
// case-sensitive volume would hold one directory for a job that runs from
// another.
func foldsCase(path string) bool {
	for {
		if _, err := os.Lstat(path); err == nil {
			break
		}
		parent := filepath.Dir(path)
		if parent == path {
			return false
		}
		path = parent
	}
	p, err := unix.BytePtrFromString(path)
	if err != nil {
		return false
	}
	al := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Volattr: unix.ATTR_VOL_CAPABILITIES}
	// The reply is a u_int32_t length, then vol_capabilities_attr_t: two
	// [4]uint32 sets, capabilities then valid. FORMAT is the first of each.
	var buf [4 + 32]byte
	// x/sys wraps setattrlist(2) through libSystem but not getattrlist(2), so
	// this goes through the numbered call, whose number has not moved since
	// it was added. A failure reads as case-sensitive.
	_, _, errno := unix.Syscall6(unix.SYS_GETATTRLIST, //nolint:staticcheck // no libSystem wrapper exists; see above
		uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&al)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
		0, 0)
	if errno != 0 || binary.LittleEndian.Uint32(buf[0:4]) < 4+32 {
		return false
	}
	caps := binary.LittleEndian.Uint32(buf[4:8])
	valid := binary.LittleEndian.Uint32(buf[20:24])
	if valid&volCapFmtCaseSensitive == 0 {
		return false
	}
	return caps&volCapFmtCaseSensitive == 0
}
