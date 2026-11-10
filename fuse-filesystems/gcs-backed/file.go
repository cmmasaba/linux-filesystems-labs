package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

var (
	errTooBig = errors.New("file exceeds --max-staged-bytes")
	errStale  = errors.New("object was replaced or deleted in GCS since it was opened")
)

// pinned translates "not found" on a generation-pinned read: the file
// handle is valid, the data it refers to is gone. That's EIO, not ENOENT.
func pinned(err error) error {
	if errors.Is(err, errNotFound) {
		return fmt.Errorf("%w: %w", errStale, err)
	}

	return err
}

// openFile is the shared state of a path that has at least one open handle.
// All handles on the same path share it, so writes through one handle are
// visible to reads through another, as on a local filesystem.
//
// Lock order: fs.nsMu, then openFile.mu, then handle.mu, then fs.mu.
// fs.mu is never held while acquiring any of the others.
type openFile struct {
	mu sync.Mutex

	// name is the current object name. Rename changes it while holding both
	// mu and fs.mu, so holding either one is enough to read it.
	name string

	gen   int64 // generation the local state is based on; condNotExist for a new file
	size  int64 // object size at gen; meaningful while !loaded
	perm  uint32
	mtime time.Time
	ctime time.Time

	data     []byte // staged contents once loaded
	loaded   bool
	dirty    bool
	unlinked bool

	// writers counts open writable handles. While any exist, gen is their
	// base and must not move: it's what makes their upload fail instead of
	// overwriting another client's newer version.
	writers int

	refs int // open handles; guarded by fs.mu
}

// handle is one open file descriptor as seen by FUSE.
type handle struct {
	f        *openFile
	append   bool
	writable bool

	// readMu serializes remote reads on this handle (the kernel sends
	// readahead concurrently), keeping them in order for the shared stream.
	// It is held during network I/O, so nothing else may wait on it.
	// Order: readMu before mu.
	readMu sync.Mutex

	mu sync.Mutex
	// Remote reads are pinned to the generation seen at open. Rename updates
	// name and gen together.
	name   string
	gen    int64
	size   int64
	closed bool

	// Sequential read stream (see readRemote).
	rd      io.ReadCloser
	rdOff   int64
	cancel  context.CancelFunc
	lastEnd int64
}

func (h *handle) closeStream() {
	if h.rd != nil {
		_ = h.rd.Close()
		h.cancel()
		h.rd, h.cancel = nil, nil
	}
}

const allocUnit = 64 * 1024

// resizeBuf returns b with length size. Bytes between the old and new length
// are always zeroed, which fixes the stale-data bug (B1) in the in-memory FS.
func resizeBuf(b []byte, size int) []byte {
	if size <= len(b) {
		return b[:size]
	}

	if size <= cap(b) {
		old := len(b)
		b = b[:size]
		clear(b[old:])

		return b
	}

	newCap := max(size, 2*cap(b))
	newCap = (newCap + allocUnit - 1) / allocUnit * allocUnit
	nb := make([]byte, size, newCap)
	copy(nb, b)

	return nb
}

// ensureLoaded downloads the object into the staging buffer the first time a
// handle writes or truncates (read-modify-write). f.mu must be held.
func (fs *FileSystem) ensureLoaded(f *openFile) error {
	if f.loaded {
		return nil
	}

	if f.gen == condNotExist || f.size == 0 {
		f.data, f.loaded = nil, true
		return nil
	}

	if f.size > fs.cfg.MaxStagedBytes {
		return errTooBig
	}

	ctx, cancel := context.WithTimeout(context.Background(), fs.cfg.IOTimeout)
	defer cancel()

	r, err := fs.b.NewReader(ctx, f.name, f.gen, 0, -1)
	if err != nil {
		return pinned(err)
	}
	defer r.Close()

	buf := bytes.NewBuffer(make([]byte, 0, f.size))
	if _, err := io.Copy(buf, io.LimitReader(r, fs.cfg.MaxStagedBytes+1)); err != nil {
		return err
	}

	if int64(buf.Len()) > fs.cfg.MaxStagedBytes {
		return errTooBig
	}

	f.data, f.loaded = buf.Bytes(), true

	return nil
}

// uploadLocked writes the staged buffer as a new generation of the object.
// The generation precondition makes the upload fail, instead of overwriting,
// if anyone else changed the object since it was opened. f.mu must be held.
func (fs *FileSystem) uploadLocked(f *openFile) error {
	if !f.dirty || f.unlinked {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), fs.cfg.IOTimeout)
	defer cancel()

	a, err := fs.b.Write(ctx, f.name, f.gen, encodeMeta(f.perm, f.mtime), bytes.NewReader(f.data))
	if err != nil {
		if errors.Is(err, errPrecondition) {
			slog.Error("upload rejected: object changed in GCS since it was opened; local changes NOT saved",
				"object", f.name, "based_on_generation", f.gen)

			return fmt.Errorf("conflict: %w", err)
		}

		return err
	}

	f.gen, f.size, f.ctime, f.dirty = a.Generation, a.Size, a.Updated, false
	fs.cache.invalidate(f.name)

	return nil
}

// readRemote serves a read from GCS for a handle whose file isn't staged.
// Sequential reads share one streaming HTTP response; anything else uses a
// bounded range request so random access doesn't open (and abandon) a stream
// to the end of the object on every read.
//
// h.mu is not held during network I/O: the stream is checked out of the
// handle and put back afterwards. A slow read therefore can't stall a rename,
// which needs h.mu to repoint the handle.
func (fs *FileSystem) readRemote(h *handle, buf []byte, off int64) int {
	h.readMu.Lock()
	defer h.readMu.Unlock()

	h.mu.Lock()

	if off >= h.size {
		h.mu.Unlock()
		return 0
	}

	n := min(int64(len(buf)), h.size-off)
	name, gen := h.name, h.gen
	sequential := off == h.lastEnd
	rd, rdOff, cancel := h.rd, h.rdOff, h.cancel
	h.rd, h.cancel = nil, nil
	h.mu.Unlock()

	if rd != nil && rdOff != off {
		_ = rd.Close()
		cancel()
		rd = nil
	}

	if rd == nil && sequential && fs.cfg.ReadStreams {
		ctx, c := context.WithCancel(context.Background())

		r, err := fs.b.NewReader(ctx, name, gen, off, -1)
		if err != nil {
			c()
			return fs.errno("read", name, pinned(err))
		}

		rd, cancel = r, c
	}

	var got int
	var err error

	if rd != nil {
		// Cancel the stream if it stalls, so the caller gets EIO instead of
		// hanging in the kernel.
		timer := time.AfterFunc(fs.cfg.IOTimeout, cancel)
		got, err = io.ReadFull(rd, buf[:n])
		timer.Stop()
	} else {
		ctx, c := context.WithTimeout(context.Background(), fs.cfg.IOTimeout)
		defer c()

		var r io.ReadCloser

		r, err = fs.b.NewReader(ctx, name, gen, off, n)
		if err == nil {
			got, err = io.ReadFull(r, buf[:n])
			_ = r.Close()
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	// Put the stream back only if it is still good and still describes this
	// handle (a rename or release may have happened meanwhile).
	if rd != nil {
		if err == nil && !h.closed && h.rd == nil && h.name == name && h.gen == gen {
			h.rd, h.rdOff, h.cancel = rd, off+int64(got), cancel
		} else {
			_ = rd.Close()
			cancel()
		}
	}

	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return fs.errno("read", name, pinned(err))
	}

	h.lastEnd = off + int64(got)

	return got
}
