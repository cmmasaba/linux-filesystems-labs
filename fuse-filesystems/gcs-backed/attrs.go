package main

import (
	"strconv"
	"time"

	"github.com/winfsp/cgofuse/fuse"
)

// Custom metadata keys. GCS stores them as x-goog-meta-gcsfs-*.
const (
	metaMode  = "gcsfs-mode"  // permission bits, octal
	metaMtime = "gcsfs-mtime" // modification time, unix nanoseconds
)

const blockSize = 4096

func encodeMeta(perm uint32, mtime time.Time) map[string]string {
	return map[string]string{
		metaMode:  modeString(perm),
		metaMtime: mtimeString(mtime),
	}
}

func modeString(perm uint32) string {
	return strconv.FormatUint(uint64(perm&0o7777), 8)
}

func mtimeString(t time.Time) string {
	return strconv.FormatInt(t.UnixNano(), 10)
}

// metaPerm returns the stored permission bits, or def if absent or invalid.
func metaPerm(meta map[string]string, def uint32) uint32 {
	v, err := strconv.ParseUint(meta[metaMode], 8, 32)
	if err != nil || v > 0o7777 {
		return def
	}

	return uint32(v)
}

// metaMtimeOr returns the stored mtime, or fallback if absent or invalid.
func metaMtimeOr(meta map[string]string, fallback time.Time) time.Time {
	v, err := strconv.ParseInt(meta[metaMtime], 10, 64)
	if err != nil {
		return fallback
	}

	return time.Unix(0, v)
}

// makeStat builds a Stat_t. Ino stays 0: without SetUseIno, libfuse assigns
// inode numbers itself. atime is not tracked and mirrors mtime.
func (fs *FileSystem) makeStat(mode uint32, size int64, mtime, ctime, birth time.Time) fuse.Stat_t {
	return fuse.Stat_t{
		Mode:     mode,
		Nlink:    1,
		Uid:      fs.cfg.UID,
		Gid:      fs.cfg.GID,
		Size:     size,
		Atim:     fuse.NewTimespec(mtime),
		Mtim:     fuse.NewTimespec(mtime),
		Ctim:     fuse.NewTimespec(ctime),
		Birthtim: fuse.NewTimespec(birth),
		Blksize:  blockSize,
		Blocks:   (size + 511) / 512,
	}
}

func (fs *FileSystem) fileStat(a *ObjAttrs) fuse.Stat_t {
	perm := metaPerm(a.Metadata, fs.cfg.FileMode)
	mtime := metaMtimeOr(a.Metadata, a.Updated)

	return fs.makeStat(fuse.S_IFREG|perm, a.Size, mtime, a.Updated, a.Created)
}

// dirStat builds a directory stat from its placeholder object, or from
// defaults for the root and implicit directories (a == nil).
func (fs *FileSystem) dirStat(a *ObjAttrs) fuse.Stat_t {
	if a == nil {
		t := fs.mountTime
		return fs.makeStat(fuse.S_IFDIR|fs.cfg.DirMode, 0, t, t, t)
	}

	perm := metaPerm(a.Metadata, fs.cfg.DirMode)
	mtime := metaMtimeOr(a.Metadata, a.Updated)

	return fs.makeStat(fuse.S_IFDIR|perm, 0, mtime, a.Updated, a.Created)
}

func (fs *FileSystem) entryStat(e entry) fuse.Stat_t {
	if e.isDir {
		return fs.dirStat(e.attrs)
	}

	return fs.fileStat(e.attrs)
}
