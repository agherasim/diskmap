package main

/*
#include <sys/attr.h>
#include <unistd.h>
static int bulk(int fd, void *buf, size_t sz) {
	struct attrlist al = {0};
	al.bitmapcount = ATTR_BIT_MAP_COUNT;
	al.commonattr = ATTR_CMN_RETURNED_ATTRS | ATTR_CMN_NAME | ATTR_CMN_DEVID | ATTR_CMN_OBJTYPE | ATTR_CMN_MODTIME | ATTR_CMN_FILEID;
	al.fileattr = ATTR_FILE_LINKCOUNT | ATTR_FILE_ALLOCSIZE;
	return getattrlistbulk(fd, &al, buf, sz, FSOPT_PACK_INVAL_ATTRS);
}
*/
import "C"

import (
	"encoding/binary"
	"errors"
	"syscall"
	"unsafe"
)

const objTypeDir = 2 // VDIR

// listDir returns every entry of a directory with the attributes we need in
// one getattrlistbulk(2) call per buffer-full, instead of one lstat per file.
// Falls back to readdir+lstat on filesystems that don't support it.
func listDir(path string, buf []byte) ([]entry, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(fd)

	// Entry layout (common attrs are always packed thanks to FSOPT_PACK_INVAL_ATTRS):
	//  0 len u32 | 4 returned attrs 5*u32 (fileattr at 16) | 24 name attrref (i32 off, u32 len)
	// 32 devid i32 | 36 objtype u32 | 40 modtime timespec (i64 sec, i64 nsec) | 56 fileid u64
	// File attrs follow only when returned (never for directories):
	// 64 linkcount u32 | 68 allocsize i64
	const fileAttrs = C.ATTR_FILE_LINKCOUNT | C.ATTR_FILE_ALLOCSIZE
	le := binary.LittleEndian
	var out []entry
	for {
		n, errno := C.bulk(C.int(fd), unsafe.Pointer(&buf[0]), C.size_t(len(buf)))
		if n < 0 {
			if errors.Is(errno, syscall.ENOTSUP) || errors.Is(errno, syscall.EINVAL) {
				return listDirStat(path)
			}
			return out, errno
		}
		if n == 0 {
			return out, nil
		}
		off := 0
		for i := 0; i < int(n); i++ {
			ln := int(le.Uint32(buf[off:]))
			e := buf[off : off+ln]
			nameAt := 24 + int(int32(le.Uint32(e[24:])))
			nameLen := int(le.Uint32(e[28:])) // includes the trailing NUL
			ent := entry{
				name:  string(e[nameAt : nameAt+nameLen-1]),
				dev:   int64(int32(le.Uint32(e[32:]))),
				dir:   le.Uint32(e[36:]) == objTypeDir,
				mtime: int64(le.Uint64(e[40:])),
				ino:   le.Uint64(e[56:]),
			}
			if le.Uint32(e[16:])&fileAttrs == fileAttrs && ln >= 76 {
				ent.nlink = le.Uint32(e[64:])
				ent.size = int64(le.Uint64(e[68:]))
			}
			out = append(out, ent)
			off += ln
		}
	}
}
