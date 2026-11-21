package workspace

import (
	"encoding/binary"
	"unsafe"

	"golang.org/x/sys/unix"
)

// attrCmnextPrivateSize is ATTR_CMNEXT_PRIVATESIZE from <sys/attr.h>: the
// bytes of a file that no APFS clone of it shares. x/sys does not export it.
const attrCmnextPrivateSize = 0x00000008

// privateBytes is how much of the regular file at path only it holds on disk.
// APFS lets two files share extents (clonefile(2), `cp -c`, pnpm's and uv's
// default import on a Mac), and st_blocks counts shared extents in full for
// each of them: a node_modules cloned from the pnpm store reports hundreds of
// megabytes and frees almost nothing when deleted. ok is false when the
// filesystem cannot say, and the caller falls back to st_blocks.
func privateBytes(path string) (n int64, ok bool) {
	p, err := unix.BytePtrFromString(path)
	if err != nil {
		return 0, false
	}
	al := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Forkattr: attrCmnextPrivateSize}
	// The reply is a u_int32_t length, then the one off_t asked for.
	var buf [16]byte
	// x/sys wraps setattrlist(2) through libSystem but not getattrlist(2), so
	// this goes through the numbered call, whose number has not moved since
	// it was added. A failure only falls back to st_blocks.
	_, _, errno := unix.Syscall6(unix.SYS_GETATTRLIST, //nolint:staticcheck // no libSystem wrapper exists; see above
		uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&al)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
		unix.FSOPT_NOFOLLOW|unix.FSOPT_ATTR_CMN_EXTENDED, 0)
	if errno != 0 || binary.LittleEndian.Uint32(buf[0:4]) < 12 {
		return 0, false
	}
	return int64(binary.LittleEndian.Uint64(buf[4:12])), true
}
