package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/winfsp/cgofuse/fuse"
)

// noFH is the file handle cgofuse passes for path-based calls.
const noFH = ^uint64(0)

// Config holds the mount options.
type Config struct {
	ImplicitDirs   bool          // treat prefixes without a placeholder object as directories
	StatTTL        time.Duration // lookup cache TTL; 0 disables
	RenameDirLimit int           // max objects a directory rename copies; EXDEV above
	MaxStagedBytes int64         // largest file that can be written
	ReadOnly       bool
	ReadStreams    bool          // stream sequential reads instead of one request per read
	OpTimeout      time.Duration // metadata and listing calls
	IOTimeout      time.Duration // uploads, downloads and read stalls
	FileMode       uint32        // default permissions when metadata is absent
	DirMode        uint32
	UID, GID       uint32 // owner of every file (decision D5)
}

// FileSystem maps a GCS bucket onto a POSIX tree: one object per file and
// "/"-delimited prefixes as directories. GCS is the source of truth; the
// process keeps short-lived lookup caches and stages writes in memory until
// close/fsync.
type FileSystem struct {
	fuse.FileSystemBase

	b         Bucket
	cfg       Config
	cache     *statCache
	now       func() time.Time
	mountTime time.Time

	// nsMu serializes namespace changes (create, mkdir, rmdir, unlink,
	// rename) so two local operations can't race on the same names. Reads,
	// getattr and readdir don't take it.
	nsMu sync.Mutex

	// mu guards the maps below and openFile.refs. It is held only for map
	// access, never during a GCS call.
	mu      sync.Mutex
	handles map[uint64]*handle
	files   map[string]*openFile
	nextFH  uint64
}

func NewFileSystem(b Bucket, cfg Config) *FileSystem {
	return &FileSystem{
		b:         b,
		cfg:       cfg,
		cache:     newStatCache(cfg.StatTTL, time.Now),
		now:       time.Now,
		mountTime: time.Now(),
		handles:   map[uint64]*handle{},
		files:     map[string]*openFile{},
	}
}

func (fs *FileSystem) opCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), fs.cfg.OpTimeout)
}

// errno maps an error to a negative errno and logs anything unexpected.
func (fs *FileSystem) errno(op, name string, err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errStale): // wraps errNotFound, so check it first
		slog.Warn("open file's data changed in GCS", "op", op, "object", name, "err", err)
		return -fuse.EIO
	case errors.Is(err, errNotFound):
		return -fuse.ENOENT
	case errors.Is(err, errTooBig):
		return -fuse.EFBIG
	case errors.Is(err, errPermission):
		slog.Warn("permission denied by GCS", "op", op, "object", name, "err", err)
		return -fuse.EACCES
	default:
		slog.Warn("operation failed", "op", op, "object", name, "err", err)
		return -fuse.EIO
	}
}

// ---- lookup ----

// lookup resolves a name with two concurrent requests: a stat of the object
// itself and a one-entry listing under name+"/". A directory wins over a file
// of the same name.
func (fs *FileSystem) lookup(name string) (entry, error) {
	if name == "" {
		return entry{exists: true, isDir: true}, nil
	}

	if !validName(name) {
		return entry{}, nil
	}

	if e, ok := fs.cache.get(name); ok {
		return e, nil
	}

	epoch := fs.cache.snapshot()

	ctx, cancel := fs.opCtx()
	defer cancel()

	var (
		wg      sync.WaitGroup
		fileA   ObjAttrs
		fileErr error
		lst     ListResult
		lstErr  error
	)

	wg.Add(2)

	go func() { defer wg.Done(); fileA, fileErr = fs.b.Stat(ctx, name) }()
	go func() { defer wg.Done(); lst, lstErr = fs.b.List(ctx, dirPrefix(name), "/", 1) }()

	wg.Wait()

	if lstErr != nil {
		return entry{}, lstErr
	}

	if fileErr != nil && !errors.Is(fileErr, errNotFound) {
		return entry{}, fileErr
	}

	var e entry

	switch {
	case len(lst.Objects) > 0 && lst.Objects[0].Name == dirPrefix(name):
		e = entry{exists: true, isDir: true, attrs: &lst.Objects[0]}
	case fs.cfg.ImplicitDirs && len(lst.Objects)+len(lst.Prefixes) > 0:
		e = entry{exists: true, isDir: true}
	case fileErr == nil:
		e = entry{exists: true, attrs: &fileA}
	}

	fs.cache.putIfUnchanged(name, e, epoch)

	return e, nil
}

// freshLookup bypasses the cache; used before changing the namespace.
func (fs *FileSystem) freshLookup(name string) (entry, error) {
	fs.cache.invalidate(name)
	return fs.lookup(name)
}

func (fs *FileSystem) openFileFor(name string) *openFile {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	return fs.files[name]
}

func (fs *FileSystem) handle(fh uint64) *handle {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	return fs.handles[fh]
}

// localStat reports an open file's local state. f.mu must be held.
func (fs *FileSystem) localStat(f *openFile) fuse.Stat_t {
	size := f.size
	if f.loaded {
		size = int64(len(f.data))
	}

	return fs.makeStat(fuse.S_IFREG|f.perm, size, f.mtime, f.ctime, f.ctime)
}

// localOverride returns the local stat of an open file whose state may differ
// from GCS (new, staged or dirty). Local state wins while a file is open.
func (fs *FileSystem) localOverride(name string) (fuse.Stat_t, bool) {
	f := fs.openFileFor(name)
	if f == nil {
		return fuse.Stat_t{}, false
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.loaded || f.dirty || f.gen == condNotExist {
		return fs.localStat(f), true
	}

	return fuse.Stat_t{}, false
}

// ---- attributes ----

func (fs *FileSystem) Getattr(path string, stat *fuse.Stat_t, fh uint64) int {
	name := objectName(path)

	if fh != noFH {
		if h := fs.handle(fh); h != nil {
			h.f.mu.Lock()
			*stat = fs.localStat(h.f)

			if !h.f.loaded { // this handle reads its own pinned generation
				h.mu.Lock()
				stat.Size, stat.Blocks = h.size, (h.size+511)/512
				h.mu.Unlock()
			}
			h.f.mu.Unlock()

			return 0
		}
	}

	if st, ok := fs.localOverride(name); ok {
		*stat = st
		return 0
	}

	e, err := fs.lookup(name)
	if err != nil {
		return fs.errno("getattr", name, err)
	}

	if !e.exists {
		return -fuse.ENOENT
	}

	*stat = fs.entryStat(e)

	return 0
}

func (fs *FileSystem) Statfs(_ string, stat *fuse.Statfs_t) int {
	// GCS has no capacity limit. Report large constant numbers so df works
	// and tools don't conclude the disk is full.
	*stat = fuse.Statfs_t{
		Bsize:   blockSize,
		Frsize:  blockSize,
		Blocks:  1 << 36,
		Bfree:   1 << 36,
		Bavail:  1 << 36,
		Files:   1 << 32,
		Ffree:   1 << 32,
		Favail:  1 << 32,
		Namemax: maxComponentLen,
	}

	return 0
}

// updateMeta applies metadata changes to a name: to the staged state of an
// open file, and in GCS to the file object or directory placeholder. Implicit
// directories and the root have nowhere to store metadata; changes to them
// are accepted and dropped.
func (fs *FileSystem) updateMeta(op, name string, local func(*openFile), meta map[string]string) int {
	if fs.cfg.ReadOnly {
		return -fuse.EROFS
	}

	if name == "" {
		return 0
	}

	if f := fs.openFileFor(name); f != nil {
		f.mu.Lock()
		defer f.mu.Unlock()

		local(f)
		f.ctime = fs.now()

		// Unsaved files pick up the change at their next upload.
		if f.dirty || f.gen == condNotExist {
			return 0
		}
	}

	e, err := fs.freshLookup(name)
	if err != nil {
		return fs.errno(op, name, err)
	}

	if !e.exists {
		return -fuse.ENOENT
	}

	if e.attrs == nil {
		slog.Debug("metadata change on implicit directory ignored", "op", op, "dir", name)
		return 0
	}

	ctx, cancel := fs.opCtx()
	defer cancel()

	_, err = fs.b.UpdateMetadata(ctx, e.attrs.Name, meta)
	fs.cache.invalidate(name)

	return fs.errno(op, name, err)
}

func (fs *FileSystem) Chmod(path string, mode uint32) int {
	perm := mode & 0o7777

	return fs.updateMeta("chmod", objectName(path),
		func(f *openFile) { f.perm = perm },
		map[string]string{metaMode: modeString(perm)})
}

func (fs *FileSystem) Utimens(path string, tmsp []fuse.Timespec) int {
	mtime := fs.now()

	if tmsp != nil {
		switch tmsp[1].Nsec {
		case fuse.UTIME_OMIT:
			return 0 // only atime was set, and atime isn't stored
		case fuse.UTIME_NOW:
		default:
			mtime = tmsp[1].Time()
		}
	}

	return fs.updateMeta("utimens", objectName(path),
		func(f *openFile) { f.mtime = mtime },
		map[string]string{metaMtime: mtimeString(mtime)})
}

// Chown refuses ownership changes: every file belongs to the mounting user
// (decision D5). Requests that don't change anything succeed so tools like
// `cp -p` work.
func (fs *FileSystem) Chown(_ string, uid, gid uint32) int {
	if (uid == ^uint32(0) || uid == fs.cfg.UID) && (gid == ^uint32(0) || gid == fs.cfg.GID) {
		return 0
	}

	return -fuse.EPERM
}

// ---- directories ----

func (fs *FileSystem) Opendir(path string) (int, uint64) {
	name := objectName(path)

	e, err := fs.lookup(name)
	if err != nil {
		return fs.errno("opendir", name, err), noFH
	}

	switch {
	case !e.exists:
		return -fuse.ENOENT, noFH
	case !e.isDir:
		return -fuse.ENOTDIR, noFH
	}

	return 0, noFH
}

func (fs *FileSystem) Releasedir(string, uint64) int { return 0 }

func (fs *FileSystem) Readdir(path string, fill func(string, *fuse.Stat_t, int64) bool, _ int64, _ uint64) int {
	name := objectName(path)
	prefix := dirPrefix(name)
	epoch := fs.cache.snapshot()

	ctx, cancel := fs.opCtx()
	defer cancel()

	lst, err := fs.b.List(ctx, prefix, "/", 0)
	if err != nil {
		return fs.errno("readdir", name, err)
	}

	// st == nil sends the entry without attributes (readdirplus), so the
	// kernel looks it up instead of trusting a guess.
	type dirent struct {
		st    *fuse.Stat_t
		isDir bool
	}

	entries := map[string]dirent{}

	// Prefixes first: a directory wins over a file with the same name.
	for _, p := range lst.Prefixes {
		child, ok := childName(prefix, p)
		if !ok {
			slog.Debug("hiding object prefix with a non-POSIX name", "prefix", p)
			continue
		}

		full := dirPrefix(name) + child

		if fs.cfg.ImplicitDirs {
			// Every prefix is a directory, but the listing doesn't say
			// whether it has a placeholder with a stored mode. Report
			// attributes only if they're already known.
			var st *fuse.Stat_t
			if e, ok := fs.cache.get(full); ok && e.exists && e.isDir {
				s := fs.dirStat(e.attrs)
				st = &s
			}

			entries[child] = dirent{st, true}

			continue
		}

		// Without implicit directories only placeholder-backed prefixes are
		// directories, exactly as lookup decides.
		e, err := fs.lookup(full)
		if err != nil {
			return fs.errno("readdir", full, err)
		}

		if e.exists && e.isDir {
			s := fs.dirStat(e.attrs)
			entries[child] = dirent{&s, true}
		}
	}

	for i := range lst.Objects {
		o := &lst.Objects[i]
		if o.Name == prefix {
			continue // this directory's own placeholder
		}

		child, ok := childName(prefix, o.Name)
		if !ok {
			slog.Debug("hiding object with a non-POSIX name", "object", o.Name)
			continue
		}

		if _, dup := entries[child]; dup {
			slog.Warn("object hidden by a directory of the same name", "object", o.Name)
			continue
		}

		st := fs.fileStat(o)
		entries[child] = dirent{&st, false}
		fs.cache.putIfUnchanged(o.Name, entry{exists: true, attrs: o}, epoch)
	}

	// Merge open files whose local state isn't in GCS yet.
	fs.mu.Lock()

	var local []*openFile

	for n, f := range fs.files {
		if parentOf(n) == name && n != name {
			local = append(local, f)
		}
	}

	fs.mu.Unlock()

	for _, f := range local {
		f.mu.Lock()

		// Skip files renamed out of this directory since the snapshot.
		if parentOf(f.name) == name {
			child := strings.TrimPrefix(f.name, prefix)
			if existing, ok := entries[child]; !ok || !existing.isDir {
				st := fs.localStat(f)
				entries[child] = dirent{&st, false}
			}
		}
		f.mu.Unlock()
	}

	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}

	sort.Strings(names)

	fill(".", nil, 0)
	fill("..", nil, 0)

	for _, n := range names {
		if !fill(n, entries[n].st, 0) {
			break
		}
	}

	return 0
}

func (fs *FileSystem) Mkdir(path string, mode uint32) int {
	if fs.cfg.ReadOnly {
		return -fuse.EROFS
	}

	name := objectName(path)
	if rc := checkNewName(name); rc != 0 {
		return rc
	}

	fs.nsMu.Lock()
	defer fs.nsMu.Unlock()

	if fs.openFileFor(name) != nil {
		return -fuse.EEXIST
	}

	e, err := fs.freshLookup(name)
	if err != nil {
		return fs.errno("mkdir", name, err)
	}

	if e.exists {
		return -fuse.EEXIST
	}

	ctx, cancel := fs.opCtx()
	defer cancel()

	_, err = fs.b.Write(ctx, dirPrefix(name), condNotExist, encodeMeta(mode&0o7777, fs.now()), bytes.NewReader(nil))
	fs.cache.invalidate(name)

	if errors.Is(err, errPrecondition) {
		return -fuse.EEXIST
	}

	return fs.errno("mkdir", name, err)
}

func (fs *FileSystem) Rmdir(path string) int {
	if fs.cfg.ReadOnly {
		return -fuse.EROFS
	}

	name := objectName(path)
	if name == "" {
		return -fuse.EBUSY
	}

	fs.nsMu.Lock()
	defer fs.nsMu.Unlock()

	fs.mu.Lock()

	for n := range fs.files {
		if parentOf(n) == name {
			fs.mu.Unlock()
			return -fuse.ENOTEMPTY
		}
	}

	fs.mu.Unlock()

	ctx, cancel := fs.opCtx()
	defer cancel()

	// Two entries are enough: the placeholder (always first if it exists)
	// and anything else.
	lst, err := fs.b.List(ctx, dirPrefix(name), "/", 2)
	if err != nil {
		return fs.errno("rmdir", name, err)
	}

	hasPlaceholder := false

	for _, o := range lst.Objects {
		if o.Name != dirPrefix(name) {
			return -fuse.ENOTEMPTY
		}

		hasPlaceholder = true
	}

	if len(lst.Prefixes) > 0 {
		return -fuse.ENOTEMPTY
	}

	if !hasPlaceholder {
		return -fuse.ENOENT
	}

	// Another client could add a child between the listing and this delete;
	// the child then lives on in an implicit directory. Accepted for the lab.
	err = fs.b.Delete(ctx, dirPrefix(name), condNone)
	fs.cache.invalidateTree(name)

	if errors.Is(err, errNotFound) {
		return 0
	}

	return fs.errno("rmdir", name, err)
}

// ---- files ----

func (fs *FileSystem) addHandle(h *handle) uint64 {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	fs.nextFH++
	fs.handles[fs.nextFH] = h

	return fs.nextFH
}

func (fs *FileSystem) newHandle(f *openFile, flags int) *handle {
	return &handle{
		f:      f,
		append: flags&fuse.O_APPEND != 0,
		name:   f.name,
		gen:    f.gen,
		size:   f.size,
	}
}

// Create makes a new, empty file. Nothing is written to GCS until the first
// flush (decision D3); until then the file exists only in this process.
func (fs *FileSystem) Create(path string, flags int, mode uint32) (int, uint64) {
	if fs.cfg.ReadOnly {
		return -fuse.EROFS, noFH
	}

	name := objectName(path)
	if rc := checkNewName(name); rc != 0 {
		return rc, noFH
	}

	fs.nsMu.Lock()
	defer fs.nsMu.Unlock()

	if fs.openFileFor(name) != nil {
		return -fuse.EEXIST, noFH
	}

	e, err := fs.freshLookup(name)
	if err != nil {
		return fs.errno("create", name, err), noFH
	}

	if e.exists {
		return -fuse.EEXIST, noFH
	}

	now := fs.now()
	f := &openFile{
		name: name, gen: condNotExist, perm: mode & 0o7777,
		mtime: now, ctime: now, loaded: true, dirty: true, refs: 1,
	}

	h := fs.newHandle(f, flags)
	if flags&fuse.O_ACCMODE != fuse.O_RDONLY {
		h.writable = true
		f.writers = 1
	}

	fs.mu.Lock()
	fs.files[name] = f
	fs.mu.Unlock()

	return 0, fs.addHandle(h)
}

// Mknod only supports regular files; GCS has nowhere to keep device numbers
// or FIFOs. Without a handle there's no later flush, so the object is written
// immediately.
func (fs *FileSystem) Mknod(path string, mode uint32, _ uint64) int {
	if fs.cfg.ReadOnly {
		return -fuse.EROFS
	}

	if mode&fuse.S_IFMT != fuse.S_IFREG {
		return -fuse.EPERM
	}

	name := objectName(path)
	if rc := checkNewName(name); rc != 0 {
		return rc
	}

	fs.nsMu.Lock()
	defer fs.nsMu.Unlock()

	if fs.openFileFor(name) != nil {
		return -fuse.EEXIST
	}

	if e, err := fs.freshLookup(name); err != nil {
		return fs.errno("mknod", name, err)
	} else if e.exists {
		return -fuse.EEXIST
	}

	ctx, cancel := fs.opCtx()
	defer cancel()

	_, err := fs.b.Write(ctx, name, condNotExist, encodeMeta(mode&0o7777, fs.now()), bytes.NewReader(nil))
	fs.cache.invalidate(name)

	if errors.Is(err, errPrecondition) {
		return -fuse.EEXIST
	}

	return fs.errno("mknod", name, err)
}

// Open stats the object on every open (close-to-open consistency) and pins
// the handle's remote reads to that generation.
func (fs *FileSystem) Open(path string, flags int) (int, uint64) {
	name := objectName(path)
	writable := flags&fuse.O_ACCMODE != fuse.O_RDONLY

	if writable && fs.cfg.ReadOnly {
		return -fuse.EROFS, noFH
	}

	f, fresh, rc := fs.acquireFile(name)
	if rc != 0 {
		return rc, noFH
	}

	// Build and register the handle under f.mu, so a concurrent rename
	// either sees it and repoints it, or finishes first and we copy the new
	// name from f.
	f.mu.Lock()
	defer f.mu.Unlock()

	h := fs.newHandle(f, flags)

	// Remote reads see the version current at this open, even when other
	// handles on the same file are pinned to an older one.
	if fresh != nil && !f.loaded {
		h.gen, h.size = fresh.Generation, fresh.Size
	}

	if writable {
		h.writable = true
		f.writers++
	}

	if writable && flags&fuse.O_TRUNC != 0 {
		f.data, f.loaded, f.dirty = f.data[:0], true, true
		f.mtime = fs.now()
	}

	return 0, fs.addHandle(h)
}

// acquireFile returns the shared openFile for name with refs incremented,
// plus the object's current attributes (nil when local state wins).
// Opening re-stats the object (close-to-open consistency). The shared state
// is rebased onto the new version only when no writer is open; see
// openFile.writers.
func (fs *FileSystem) acquireFile(name string) (*openFile, *ObjAttrs, int) {
	fs.mu.Lock()
	f := fs.files[name]

	if f != nil {
		f.refs++
	}

	fs.mu.Unlock()

	if f != nil {
		f.mu.Lock()
		local := f.loaded || f.dirty || f.gen == condNotExist
		f.mu.Unlock()

		if local {
			return f, nil, 0
		}
	}

	epoch := fs.cache.snapshot()

	ctx, cancel := fs.opCtx()
	defer cancel()

	a, err := fs.b.Stat(ctx, name)
	if err != nil {
		if f != nil {
			fs.release(f)
		}

		return nil, nil, fs.errno("open", name, err)
	}

	fs.cache.putIfUnchanged(name, entry{exists: true, attrs: &a}, epoch)

	if f != nil {
		f.mu.Lock()
		if f.writers == 0 && !f.loaded && !f.dirty && f.gen != condNotExist {
			fs.setRemote(f, &a)
		}
		f.mu.Unlock()

		return f, &a, 0
	}

	nf := &openFile{name: name, refs: 1}
	fs.setRemote(nf, &a)

	fs.mu.Lock()
	defer fs.mu.Unlock()

	if f := fs.files[name]; f != nil { // lost a race with another open
		f.refs++
		return f, &a, 0
	}

	fs.files[name] = nf

	return nf, &a, 0
}

func (fs *FileSystem) setRemote(f *openFile, a *ObjAttrs) {
	f.gen, f.size = a.Generation, a.Size
	f.perm = metaPerm(a.Metadata, fs.cfg.FileMode)
	f.mtime = metaMtimeOr(a.Metadata, a.Updated)
	f.ctime = a.Updated
}

// release drops one reference and forgets the file when none remain.
func (fs *FileSystem) release(f *openFile) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	f.refs--
	if f.refs == 0 && fs.files[f.name] == f {
		delete(fs.files, f.name)
	}
}

func (fs *FileSystem) Read(_ string, buf []byte, off int64, fh uint64) int {
	h := fs.handle(fh)
	if h == nil {
		return -fuse.EBADF
	}

	f := h.f
	f.mu.Lock()

	if f.loaded {
		n := 0
		if off < int64(len(f.data)) {
			n = copy(buf, f.data[off:])
		}

		f.mu.Unlock()

		return n
	}

	f.mu.Unlock()

	return fs.readRemote(h, buf, off)
}

func (fs *FileSystem) Write(_ string, buf []byte, off int64, fh uint64) int {
	if fs.cfg.ReadOnly {
		return -fuse.EROFS
	}

	h := fs.handle(fh)
	if h == nil {
		return -fuse.EBADF
	}

	f := h.f
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := fs.ensureLoaded(f); err != nil {
		return fs.errno("write", f.name, err)
	}

	if h.append {
		off = int64(len(f.data))
	}

	end := off + int64(len(buf))
	if end > fs.cfg.MaxStagedBytes {
		return -fuse.EFBIG
	}

	if end > int64(len(f.data)) {
		f.data = resizeBuf(f.data, int(end))
	}

	copy(f.data[off:], buf)
	f.dirty = true
	f.mtime = fs.now()
	f.ctime = f.mtime

	return len(buf)
}

func (fs *FileSystem) Truncate(path string, size int64, fh uint64) int {
	if fs.cfg.ReadOnly {
		return -fuse.EROFS
	}

	if size > fs.cfg.MaxStagedBytes {
		return -fuse.EFBIG
	}

	name := objectName(path)
	pathOnly := true

	var f *openFile

	if h := fs.handle(fh); h != nil {
		f, pathOnly = h.f, false
	} else if f = fs.openFileFor(name); f == nil {
		// Not open anywhere: stage it in a throwaway openFile.
		ctx, cancel := fs.opCtx()
		a, err := fs.b.Stat(ctx, name)

		cancel()

		if err != nil {
			return fs.errno("truncate", name, err)
		}

		f = &openFile{name: name}
		fs.setRemote(f, &a)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if size == 0 {
		f.data, f.loaded = f.data[:0], true
	} else if err := fs.ensureLoaded(f); err != nil {
		return fs.errno("truncate", name, err)
	}

	f.data = resizeBuf(f.data, int(size))
	f.dirty = true
	f.mtime = fs.now()
	f.ctime = f.mtime

	// truncate(2) has no close to flush on, so save it now.
	if pathOnly {
		if err := fs.uploadLocked(f); err != nil {
			return fs.errno("truncate", name, err)
		}
	}

	return 0
}

func (fs *FileSystem) Flush(_ string, fh uint64) int {
	h := fs.handle(fh)
	if h == nil {
		return 0
	}

	h.f.mu.Lock()
	defer h.f.mu.Unlock()

	return fs.errno("flush", h.f.name, fs.uploadLocked(h.f))
}

func (fs *FileSystem) Fsync(path string, _ bool, fh uint64) int {
	return fs.Flush(path, fh)
}

// Release closes a handle. Flush normally saved the data already; if the file
// is still dirty (flush failed), try once more, because after this the staged
// data is gone. The result can't reach the application, so it is only logged.
func (fs *FileSystem) Release(_ string, fh uint64) int {
	fs.mu.Lock()
	h := fs.handles[fh]
	delete(fs.handles, fh)
	fs.mu.Unlock()

	if h == nil {
		return 0
	}

	h.mu.Lock()
	h.closed = true
	h.closeStream()
	h.mu.Unlock()

	f := h.f
	f.mu.Lock()

	if h.writable {
		f.writers--
	}

	if f.dirty && !f.unlinked {
		if err := fs.uploadLocked(f); err != nil {
			slog.Error("data lost: could not save file on release", "object", f.name, "err", err)
		}
	}

	f.mu.Unlock()
	fs.release(f)

	return 0
}

// Unlink deletes the object. Open handles keep their staged data, and their
// flushes become no-ops so the file isn't recreated. Reads that still need
// GCS fail once the object is gone, unlike POSIX.
//
// The open file stays locked across the delete (so no flush can recreate the
// object mid-unlink) and is detached only once the delete has succeeded; a
// failed unlink leaves everything as it was.
func (fs *FileSystem) Unlink(path string) int {
	if fs.cfg.ReadOnly {
		return -fuse.EROFS
	}

	name := objectName(path)

	fs.nsMu.Lock()
	defer fs.nsMu.Unlock()

	f := fs.openFileFor(name)
	if f != nil {
		f.mu.Lock()
		defer f.mu.Unlock()
	}

	// A file that was never uploaded has nothing in GCS to delete; an object
	// that appeared there since belongs to someone else.
	if f == nil || f.gen != condNotExist {
		ctx, cancel := fs.opCtx()
		defer cancel()

		err := fs.b.Delete(ctx, name, condNone)
		fs.cache.invalidate(name)

		if err != nil {
			return fs.errno("unlink", name, err)
		}
	}

	if f != nil {
		f.unlinked = true

		fs.mu.Lock()
		if fs.files[name] == f {
			delete(fs.files, name)
		}
		fs.mu.Unlock()
	}

	fs.cache.invalidate(name)

	return 0
}

// ---- rename ----

func (fs *FileSystem) Rename(oldpath, newpath string) int {
	if fs.cfg.ReadOnly {
		return -fuse.EROFS
	}

	oldN, newN := objectName(oldpath), objectName(newpath)

	if rc := checkNewName(newN); rc != 0 {
		return rc
	}

	if oldN == newN {
		return 0
	}

	if strings.HasPrefix(newN, oldN+"/") || oldN == "" {
		return -fuse.EINVAL // a directory can't move into itself
	}

	fs.nsMu.Lock()
	defer fs.nsMu.Unlock()

	// Open files at either name may hold changes GCS hasn't seen (including
	// files that exist only locally). Upload them first so the existence and
	// emptiness checks and the server-side copy see the real state. The
	// source stays locked until the move is complete, so its generation
	// can't change underneath the copy.
	f := fs.openFileFor(oldN)
	if f != nil {
		f.mu.Lock()
		defer f.mu.Unlock()

		if err := fs.uploadLocked(f); err != nil {
			return fs.errno("rename", oldN, err)
		}
	}

	if d := fs.openFileFor(newN); d != nil && d != f {
		d.mu.Lock()
		err := fs.uploadLocked(d)
		d.mu.Unlock()

		if err != nil {
			return fs.errno("rename", newN, err)
		}
	}

	src, err := fs.freshLookup(oldN)
	if err != nil {
		return fs.errno("rename", oldN, err)
	}

	if !src.exists {
		return -fuse.ENOENT
	}

	dst, err := fs.freshLookup(newN)
	if err != nil {
		return fs.errno("rename", newN, err)
	}

	if src.isDir {
		return fs.renameDir(oldN, newN, dst)
	}

	return fs.renameFile(oldN, newN, src, dst, f)
}

// renameFile copies server-side, then deletes the source. Both steps carry
// generation preconditions, so a concurrent change makes the rename fail
// instead of losing data. It is not atomic: a crash between the two steps
// leaves both objects. f is the source's open file, if any, already locked by
// Rename.
func (fs *FileSystem) renameFile(oldN, newN string, src, dst entry, f *openFile) int {
	if dst.exists && dst.isDir {
		return -fuse.EISDIR
	}

	dstCond := condNotExist
	if dst.exists {
		dstCond = dst.attrs.Generation
	}

	ctx, cancel := context.WithTimeout(context.Background(), fs.cfg.IOTimeout)
	defer cancel()

	srcGen := src.attrs.Generation

	na, err := fs.b.Copy(ctx, oldN, srcGen, newN, dstCond)
	if err != nil {
		if errors.Is(err, errPrecondition) {
			slog.Error("rename aborted: destination changed in GCS", "from", oldN, "to", newN)
		}

		return fs.errno("rename", oldN, err)
	}

	// The copy exists from here on, even if the delete below fails.
	fs.cache.invalidate(oldN, newN)

	if err := fs.b.Delete(ctx, oldN, srcGen); err != nil {
		slog.Error("rename copied the file but could not delete the source; both now exist",
			"from", oldN, "to", newN, "err", err)

		return fs.errno("rename", oldN, err)
	}

	fs.cache.invalidate(oldN, newN)
	fs.moveOpenFiles(map[string]movedObject{oldN: {newN, srcGen, na.Generation}}, map[*openFile]bool{f: f != nil})

	return 0
}

type movedObject struct {
	to     string
	oldGen int64
	newGen int64
}

// moveOpenFiles points open files and their handles at new names after a
// rename. Files open at a destination name were replaced, so they are marked
// unlinked. held is the set of files whose mutex the caller already holds;
// any other moving file is locked here. Only rename (serialized by nsMu)
// holds several openFile mutexes at once, so this can't deadlock.
func (fs *FileSystem) moveOpenFiles(moved map[string]movedObject, held map[*openFile]bool) {
	fs.mu.Lock()

	var moving, replaced []*openFile

	for from, m := range moved {
		if f := fs.files[from]; f != nil {
			moving = append(moving, f)
		}

		if d := fs.files[m.to]; d != nil {
			replaced = append(replaced, d)
		}
	}

	fs.mu.Unlock()

	for _, d := range replaced {
		d.mu.Lock()
		d.unlinked = true
		d.mu.Unlock()
	}

	for _, f := range moving {
		if !held[f] {
			f.mu.Lock()
			defer f.mu.Unlock()
		}
	}

	isMoving := map[*openFile]movedObject{}
	var handles []*handle

	fs.mu.Lock()

	for _, d := range replaced {
		if fs.files[d.name] == d {
			delete(fs.files, d.name)
		}
	}

	for _, f := range moving {
		m, ok := moved[f.name]
		if !ok || fs.files[f.name] != f {
			continue // changed while we were locking
		}

		delete(fs.files, f.name)
		f.name = m.to
		isMoving[f] = m

		// If someone opened the new name while the copy ran, keep their
		// entry: both openFiles stay valid, and generation preconditions
		// decide between their uploads (the second one gets EIO).
		if fs.files[m.to] == nil {
			fs.files[m.to] = f
		}
	}

	for _, h := range fs.handles {
		if _, ok := isMoving[h.f]; ok {
			handles = append(handles, h)
		}
	}

	fs.mu.Unlock()

	for f, m := range isMoving {
		if f.gen == m.oldGen {
			f.gen = m.newGen
		}
	}

	for _, h := range handles {
		m := isMoving[h.f]

		h.mu.Lock()
		h.closeStream()
		h.name = m.to

		if h.gen == m.oldGen {
			h.gen = m.newGen
		}
		h.mu.Unlock()
	}
}

// renameDir moves every object under the prefix: copy all, then delete all,
// with the placeholder last both times. A crash leaves duplicates, not data
// loss. Directories over --rename-dir-limit objects get EXDEV; `mv` then
// falls back to copying through the mount.
func (fs *FileSystem) renameDir(oldN, newN string, dst entry) int {
	if dst.exists && !dst.isDir {
		return -fuse.ENOTDIR
	}

	oldP, newP := dirPrefix(oldN), dirPrefix(newN)

	ctx, cancel := context.WithTimeout(context.Background(), fs.cfg.IOTimeout)
	defer cancel()

	// The destination must be empty, counting files that exist only locally.
	fs.mu.Lock()

	for n := range fs.files {
		if strings.HasPrefix(n, newP) {
			fs.mu.Unlock()
			return -fuse.ENOTEMPTY
		}
	}

	fs.mu.Unlock()

	if dst.exists {
		lst, err := fs.b.List(ctx, newP, "/", 2)
		if err != nil {
			return fs.errno("rename", newN, err)
		}

		for _, o := range lst.Objects {
			if o.Name != newP {
				return -fuse.ENOTEMPTY
			}
		}

		if len(lst.Prefixes) > 0 {
			return -fuse.ENOTEMPTY
		}
	}

	// An empty destination's placeholder is overwritten by the source's,
	// last, so a rename that fails or is refused leaves it in place.
	dstPlaceholderCond := condNotExist
	if dst.exists && dst.attrs != nil {
		dstPlaceholderCond = dst.attrs.Generation
	}

	// Save open files under the directory so the copy includes their data,
	// and keep them locked so nothing writes to them mid-move.
	fs.mu.Lock()

	held := map[*openFile]bool{}

	for n, f := range fs.files {
		if strings.HasPrefix(n, oldP) {
			held[f] = true
		}
	}

	fs.mu.Unlock()

	for f := range held {
		f.mu.Lock()
		defer f.mu.Unlock()

		if err := fs.uploadLocked(f); err != nil {
			return fs.errno("rename", f.name, err)
		}
	}

	lst, err := fs.b.List(ctx, oldP, "", fs.cfg.RenameDirLimit+1)
	if err != nil {
		return fs.errno("rename", oldN, err)
	}

	if len(lst.Objects) > fs.cfg.RenameDirLimit {
		slog.Info("directory rename refused: over --rename-dir-limit; returning EXDEV so mv copies instead",
			"dir", oldN, "limit", fs.cfg.RenameDirLimit)

		return -fuse.EXDEV
	}

	objs := lst.Objects
	sort.SliceStable(objs, func(i, j int) bool { return objs[i].Name != oldP && objs[j].Name == oldP })

	moved := map[string]movedObject{}

	for _, o := range objs {
		to := newP + strings.TrimPrefix(o.Name, oldP)

		cond := condNotExist
		if o.Name == oldP {
			cond = dstPlaceholderCond
		}

		na, err := fs.b.Copy(ctx, o.Name, o.Generation, to, cond)
		if err != nil {
			slog.Error("directory rename stopped part-way; source is intact, destination is partial",
				"from", oldN, "to", newN, "copied", len(moved), "total", len(objs), "err", err)
			fs.cache.invalidateTree(newN)

			return fs.errno("rename", o.Name, err)
		}

		moved[o.Name] = movedObject{to, o.Generation, na.Generation}
	}

	for _, o := range objs {
		if err := fs.b.Delete(ctx, o.Name, o.Generation); err != nil && !errors.Is(err, errNotFound) {
			slog.Error("directory rename copied everything but could not delete all sources",
				"from", oldN, "to", newN, "object", o.Name, "err", err)
			fs.cache.invalidateTree(oldN)
			fs.cache.invalidateTree(newN)

			return fs.errno("rename", o.Name, err)
		}
	}

	fs.cache.invalidateTree(oldN)
	fs.cache.invalidateTree(newN)
	fs.moveOpenFiles(moved, held)

	return 0
}

// Destroy runs at unmount. Save anything still dirty; there is no later chance.
func (fs *FileSystem) Destroy() {
	fs.mu.Lock()

	files := make([]*openFile, 0, len(fs.files))
	for _, f := range fs.files {
		files = append(files, f)
	}

	fs.mu.Unlock()

	for _, f := range files {
		f.mu.Lock()

		if err := fs.uploadLocked(f); err != nil {
			slog.Error("data lost at unmount", "object", f.name, "err", err)
		}

		f.mu.Unlock()
	}
}
