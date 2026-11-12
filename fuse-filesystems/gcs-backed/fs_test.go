package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/winfsp/cgofuse/fuse"
)

// testEnv is a FileSystem plus direct access to its bucket. raw writes
// bypass the filesystem, standing in for another client.
type testEnv struct {
	fs    *FileSystem
	raw   Bucket
	calls *countingBucket
}

func testConfig() Config {
	return Config{
		ImplicitDirs:   true,
		StatTTL:        0,
		RenameDirLimit: 1000,
		MaxStagedBytes: 1 << 20,
		ReadStreams:    true,
		OpTimeout:      5 * time.Second,
		IOTimeout:      5 * time.Second,
		FileMode:       0o644,
		DirMode:        0o755,
		UID:            1000,
		GID:            1000,
	}
}

func newEnv(t *testing.T, raw Bucket, mut ...func(*Config)) *testEnv {
	t.Helper()

	cfg := testConfig()
	for _, m := range mut {
		m(&cfg)
	}

	calls := newCountingBucket(raw)

	return &testEnv{fs: NewFileSystem(calls, cfg), raw: raw, calls: calls}
}

// forEachEnv runs fn against both backends.
func forEachEnv(t *testing.T, fn func(t *testing.T, e *testEnv), mut ...func(*Config)) {
	forEachBackend(t, func(t *testing.T, bk backend) {
		fn(t, newEnv(t, bk.new(t), mut...))
	})
}

func (e *testEnv) put(t *testing.T, name, data string, meta map[string]string) ObjAttrs {
	t.Helper()
	return mustWrite(t, e.raw, name, condNone, data, meta)
}

func (e *testEnv) remote(t *testing.T, name string) (string, bool) {
	t.Helper()

	got, err := readString(t, e.raw, name, 0, 0, -1)
	if errors.Is(err, errNotFound) {
		return "", false
	}

	if err != nil {
		t.Fatalf("reading %q: %v", name, err)
	}

	return got, true
}

func (e *testEnv) exists(t *testing.T, name string) bool {
	t.Helper()

	_, err := e.raw.Stat(context.Background(), name)
	if err != nil && !errors.Is(err, errNotFound) {
		t.Fatal(err)
	}

	return err == nil
}

func check(t *testing.T, what string, got, want int) {
	t.Helper()

	if got != want {
		t.Fatalf("%s = %d, want %d", what, got, want)
	}
}

func (e *testEnv) open(t *testing.T, path string, flags int) uint64 {
	t.Helper()

	rc, fh := e.fs.Open(path, flags)
	check(t, "Open("+path+")", rc, 0)

	return fh
}

// writeFile creates path with data the way `echo data > path` would:
// create, write, close (flush + release).
func (e *testEnv) writeFile(t *testing.T, path, data string) {
	t.Helper()

	rc, fh := e.fs.Create(path, fuse.O_WRONLY, 0o644)
	check(t, "Create("+path+")", rc, 0)
	check(t, "Write", e.fs.Write(path, []byte(data), 0, fh), len(data))
	check(t, "Flush", e.fs.Flush(path, fh), 0)
	check(t, "Release", e.fs.Release(path, fh), 0)
}

// readFile reads the whole file in 4 KiB chunks through a fresh handle.
func (e *testEnv) readFile(t *testing.T, path string) string {
	t.Helper()

	fh := e.open(t, path, fuse.O_RDONLY)
	defer e.fs.Release(path, fh)

	return e.readHandle(t, path, fh)
}

func (e *testEnv) readHandle(t *testing.T, path string, fh uint64) string {
	t.Helper()

	var out bytes.Buffer

	buf := make([]byte, 4096)

	for off := int64(0); ; {
		n := e.fs.Read(path, buf, off, fh)
		if n < 0 {
			t.Fatalf("Read(%s, off %d) = %d", path, off, n)
		}

		if n == 0 {
			return out.String()
		}

		out.Write(buf[:n])
		off += int64(n)
	}
}

func (e *testEnv) stat(t *testing.T, path string) fuse.Stat_t {
	t.Helper()

	var st fuse.Stat_t
	check(t, "Getattr("+path+")", e.fs.Getattr(path, &st, noFH), 0)

	return st
}

func (e *testEnv) ls(t *testing.T, path string) []string {
	t.Helper()

	var names []string

	rc := e.fs.Readdir(path, func(name string, _ *fuse.Stat_t, _ int64) bool {
		if name != "." && name != ".." {
			names = append(names, name)
		}

		return true
	}, 0, noFH)
	check(t, "Readdir("+path+")", rc, 0)

	return names
}

func isDir(st fuse.Stat_t) bool { return st.Mode&fuse.S_IFMT == fuse.S_IFDIR }

func wantNames(t *testing.T, got []string, want ...string) {
	t.Helper()

	if !slices.Equal(got, want) {
		t.Fatalf("entries = %q, want %q", got, want)
	}
}

// ---- M1: read path ----

// T-101
func TestReaddirPlaceholderImplicitAndFiles(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		e.put(t, "d1/", "", map[string]string{metaMode: "700"})
		e.put(t, "d1/f", "x", nil)
		e.put(t, "d2/deep/x", "x", nil)
		e.put(t, "top.txt", "hello", nil)

		wantNames(t, e.ls(t, "/"), "d1", "d2", "top.txt")
		wantNames(t, e.ls(t, "/d1"), "f")
		wantNames(t, e.ls(t, "/d2"), "deep")

		if st := e.stat(t, "/d1"); !isDir(st) || st.Mode&0o7777 != 0o700 {
			t.Fatalf("/d1 mode = %o, want dir 0700", st.Mode)
		}

		if st := e.stat(t, "/d2"); !isDir(st) || st.Mode&0o7777 != 0o755 {
			t.Fatalf("/d2 (implicit) mode = %o, want dir 0755", st.Mode)
		}

		if st := e.stat(t, "/top.txt"); isDir(st) || st.Size != 5 || st.Uid != 1000 {
			t.Fatalf("/top.txt stat = %+v", st)
		}

		var st fuse.Stat_t
		check(t, "Getattr(missing)", e.fs.Getattr("/nope", &st, noFH), -fuse.ENOENT)
	})
}

// T-101, implicit dirs off: only placeholder directories resolve.
func TestImplicitDirsDisabled(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		e.put(t, "explicit/", "", nil)
		e.put(t, "implicit/x", "x", nil)

		if !isDir(e.stat(t, "/explicit")) {
			t.Fatal("/explicit should be a directory")
		}

		var st fuse.Stat_t
		check(t, "Getattr(/implicit)", e.fs.Getattr("/implicit", &st, noFH), -fuse.ENOENT)
	}, func(c *Config) { c.ImplicitDirs = false })
}

// T-102: names POSIX can't represent are hidden, not fatal.
func TestNonPOSIXNamesHidden(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		long := strings.Repeat("L", 300)

		for _, n := range []string{"a//b", "x/./y", "../evil", long, "ok/" + long, "good"} {
			e.put(t, n, "x", nil)
		}

		wantNames(t, e.ls(t, "/"), "a", "good", "ok", "x")
		wantNames(t, e.ls(t, "/a"))
		wantNames(t, e.ls(t, "/x"))
		wantNames(t, e.ls(t, "/ok"))

		var st fuse.Stat_t
		check(t, "Getattr(long)", e.fs.Getattr("/"+long, &st, noFH), -fuse.ENOENT)
	})
}

// T-103: a file and a directory with the same name resolve to the directory.
func TestFileDirConflictDirectoryWins(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		e.put(t, "c", "file", nil)
		e.put(t, "c/inner", "x", nil)

		wantNames(t, e.ls(t, "/"), "c")

		if !isDir(e.stat(t, "/c")) {
			t.Fatal("/c should resolve to the directory")
		}
	})
}

// T-104: reads at assorted offsets, with and without read streams.
func TestReadOffsets(t *testing.T) {
	for _, streams := range []bool{true, false} {
		t.Run(fmt.Sprintf("streams=%v", streams), func(t *testing.T) {
			forEachEnv(t, func(t *testing.T, e *testEnv) {
				data := strings.Repeat("0123456789", 1000) // 10 000 bytes
				e.put(t, "f", data, nil)

				if got := e.readFile(t, "/f"); got != data {
					t.Fatalf("sequential read returned %d bytes, mismatch", len(got))
				}

				fh := e.open(t, "/f", fuse.O_RDONLY)
				defer e.fs.Release("/f", fh)

				cases := []struct {
					off  int64
					n    int
					want string
				}{
					{0, 4, "0123"},
					{5005, 3, "567"},
					{9998, 10, "89"}, // crosses the end
					{10000, 4, ""},   // at EOF
					{20000, 4, ""},   // past EOF
					{2, 2, "23"},     // backwards seek
				}

				for _, c := range cases {
					buf := make([]byte, c.n)

					n := e.fs.Read("/f", buf, c.off, fh)
					if n < 0 || string(buf[:n]) != c.want {
						t.Fatalf("Read(off %d, len %d) = %d %q, want %q", c.off, c.n, n, buf[:max(n, 0)], c.want)
					}
				}
			}, func(c *Config) { c.ReadStreams = streams })
		})
	}
}

// T-105: a handle never mixes generations. If the object is replaced after
// open, reads that need GCS fail instead of returning the new content.
func TestReadAfterRemoteReplaceFails(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		e.put(t, "f", "original", nil)

		fh := e.open(t, "/f", fuse.O_RDONLY)
		defer e.fs.Release("/f", fh)

		e.put(t, "f", "replaced!", nil)

		check(t, "Read after replace", e.fs.Read("/f", make([]byte, 16), 0, fh), -fuse.EIO)

		if got := e.readFile(t, "/f"); got != "replaced!" {
			t.Fatalf("fresh open reads %q, want the new content", got)
		}
	})
}

// T-106: objects written by other tools get default mode and their Updated time.
func TestDefaultsWithoutMetadata(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		a := e.put(t, "plain", "x", nil)

		st := e.stat(t, "/plain")
		if st.Mode != fuse.S_IFREG|0o644 {
			t.Fatalf("mode = %o, want regular 0644", st.Mode)
		}

		if got := st.Mtim.Time(); !got.Equal(a.Updated) {
			t.Fatalf("mtime = %v, want object Updated %v", got, a.Updated)
		}
	})
}

// T-107: a read-only mount refuses every change.
func TestReadOnlyMount(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		e.put(t, "f", "x", nil)
		e.put(t, "d/", "", nil)

		rc, _ := e.fs.Open("/f", fuse.O_RDWR)
		check(t, "Open(O_RDWR)", rc, -fuse.EROFS)

		rc, _ = e.fs.Create("/new", fuse.O_WRONLY, 0o644)
		check(t, "Create", rc, -fuse.EROFS)
		check(t, "Mkdir", e.fs.Mkdir("/d2", 0o755), -fuse.EROFS)
		check(t, "Rmdir", e.fs.Rmdir("/d"), -fuse.EROFS)
		check(t, "Unlink", e.fs.Unlink("/f"), -fuse.EROFS)
		check(t, "Rename", e.fs.Rename("/f", "/g"), -fuse.EROFS)
		check(t, "Truncate", e.fs.Truncate("/f", 0, noFH), -fuse.EROFS)
		check(t, "Chmod", e.fs.Chmod("/f", 0o600), -fuse.EROFS)

		if got := e.readFile(t, "/f"); got != "x" {
			t.Fatalf("read = %q", got)
		}
	}, func(c *Config) { c.ReadOnly = true })
}

// ---- M2: write path ----

// T-201: create, write and close; the object and its metadata are in GCS.
func TestCreateWriteReadBack(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		rc, fh := e.fs.Create("/new.txt", fuse.O_WRONLY, 0o640)
		check(t, "Create", rc, 0)
		check(t, "Write", e.fs.Write("/new.txt", []byte("hello "), 0, fh), 6)
		check(t, "Write", e.fs.Write("/new.txt", []byte("world"), 6, fh), 5)

		if e.exists(t, "new.txt") {
			t.Fatal("object uploaded before flush (decision D3 says first flush)")
		}

		check(t, "Flush", e.fs.Flush("/new.txt", fh), 0)
		check(t, "Release", e.fs.Release("/new.txt", fh), 0)

		if got, _ := e.remote(t, "new.txt"); got != "hello world" {
			t.Fatalf("object content = %q", got)
		}

		a, _ := e.raw.Stat(context.Background(), "new.txt")
		if a.Metadata[metaMode] != "640" || a.Metadata[metaMtime] == "" {
			t.Fatalf("object metadata = %v", a.Metadata)
		}

		if got := e.readFile(t, "/new.txt"); got != "hello world" {
			t.Fatalf("read back = %q", got)
		}

		if st := e.stat(t, "/new.txt"); st.Mode != fuse.S_IFREG|0o640 || st.Size != 11 {
			t.Fatalf("stat = mode %o size %d", st.Mode, st.Size)
		}
	})
}

// T-201: `touch` creates an empty object.
func TestCreateEmptyFile(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		e.writeFile(t, "/empty", "")

		if got, ok := e.remote(t, "empty"); !ok || got != "" {
			t.Fatalf("empty object: exists=%v content=%q", ok, got)
		}

		rc, _ := e.fs.Create("/empty", fuse.O_WRONLY|fuse.O_EXCL, 0o644)
		check(t, "Create(existing, O_EXCL)", rc, -fuse.EEXIST)
	})
}

// EXP-002: the kernel sends CREATE when its lookup said "missing", which can
// be out of date. On an existing name, Create must act like open(2) with
// O_CREAT.
func TestCreateExistingName(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		e.put(t, "f", "theirs", nil)

		rc, fh := e.fs.Create("/f", fuse.O_RDWR, 0o600)
		check(t, "Create(existing)", rc, 0)

		if got := e.readHandle(t, "/f", fh); got != "theirs" {
			t.Fatalf("Create without O_TRUNC reads %q, want the existing data", got)
		}

		e.fs.Release("/f", fh)

		rc, fh = e.fs.Create("/f", fuse.O_WRONLY|fuse.O_TRUNC, 0o600)
		check(t, "Create(existing, O_TRUNC)", rc, 0)
		check(t, "Write", e.fs.Write("/f", []byte("mine"), 0, fh), 4)
		check(t, "Flush", e.fs.Flush("/f", fh), 0)
		e.fs.Release("/f", fh)

		if got, _ := e.remote(t, "f"); got != "mine" {
			t.Fatalf("remote = %q, want the truncated and rewritten file", got)
		}

		// The mode argument applies only to a file that is actually created.
		if perm := e.stat(t, "/f").Mode & 0o7777; perm != 0o644 {
			t.Fatalf("mode = %o, want the existing file's 644", perm)
		}

		check(t, "Mkdir", e.fs.Mkdir("/d", 0o755), 0)
		rc, _ = e.fs.Create("/d", fuse.O_WRONLY, 0o644)
		check(t, "Create(directory)", rc, -fuse.EISDIR)
	})
}

// EXP-002, as a user sees it: a cached miss is still being served after
// another client created the file, and `echo > f` must work anyway.
func TestCreateAfterStaleCachedMiss(t *testing.T) {
	e := newEnv(t, newMemBucket(), func(c *Config) { c.StatTTL = time.Minute })

	var st fuse.Stat_t
	check(t, "Getattr", e.fs.Getattr("/f", &st, noFH), -fuse.ENOENT)

	e.put(t, "f", "theirs", nil)
	check(t, "Getattr (cached miss)", e.fs.Getattr("/f", &st, noFH), -fuse.ENOENT)

	rc, fh := e.fs.Create("/f", fuse.O_WRONLY|fuse.O_TRUNC, 0o644)
	check(t, "Create", rc, 0)
	check(t, "Write", e.fs.Write("/f", []byte("mine"), 0, fh), 4)
	check(t, "Flush", e.fs.Flush("/f", fh), 0)
	e.fs.Release("/f", fh)

	if got, _ := e.remote(t, "f"); got != "mine" {
		t.Fatalf("remote = %q, want %q", got, "mine")
	}

	rc, _ = e.fs.Create("/f", fuse.O_WRONLY|fuse.O_EXCL, 0o644)
	check(t, "Create(O_EXCL)", rc, -fuse.EEXIST)
}

// T-202: shrink then grow returns zeros (bug B1 regression).
func TestTruncateShrinkGrowZeroFills(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		e.put(t, "f", strings.Repeat("x", 50), nil)

		fh := e.open(t, "/f", fuse.O_RDWR)
		check(t, "Truncate(10)", e.fs.Truncate("/f", 10, fh), 0)
		check(t, "Truncate(100)", e.fs.Truncate("/f", 100, fh), 0)
		check(t, "Flush", e.fs.Flush("/f", fh), 0)
		e.fs.Release("/f", fh)

		want := strings.Repeat("x", 10) + strings.Repeat("\x00", 90)
		if got, _ := e.remote(t, "f"); got != want {
			t.Fatalf("content = %q", got)
		}
	})
}

// T-202: truncate(2) on a file nobody has open is saved immediately.
func TestPathTruncate(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		e.put(t, "f", "0123456789", map[string]string{metaMode: "600"})

		check(t, "Truncate(4)", e.fs.Truncate("/f", 4, noFH), 0)

		if got, _ := e.remote(t, "f"); got != "0123" {
			t.Fatalf("after truncate(4): %q", got)
		}

		check(t, "Truncate(0)", e.fs.Truncate("/f", 0, noFH), 0)

		if got, _ := e.remote(t, "f"); got != "" {
			t.Fatalf("after truncate(0): %q", got)
		}

		if st := e.stat(t, "/f"); st.Mode&0o7777 != 0o600 {
			t.Fatalf("truncate lost the mode: %o", st.Mode)
		}
	})
}

// T-203: if someone else changed the object since open, the upload is
// rejected (EIO from close) and their version survives.
func TestConflictingWriteFails(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		e.put(t, "f", "v1", nil)

		fh := e.open(t, "/f", fuse.O_WRONLY)
		e.put(t, "f", "theirs", nil)

		check(t, "Write", e.fs.Write("/f", []byte("mine"), 0, fh), -fuse.EIO)
		e.fs.Release("/f", fh)

		if got, _ := e.remote(t, "f"); got != "theirs" {
			t.Fatalf("remote = %q, want the other writer's version", got)
		}
	})
}

// T-203: same, when the other writer lands after our data was staged.
func TestConflictAtFlush(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		e.put(t, "f", "v1", nil)

		fh := e.open(t, "/f", fuse.O_WRONLY)
		check(t, "Write", e.fs.Write("/f", []byte("mine"), 0, fh), 4)

		e.put(t, "f", "theirs", nil)

		check(t, "Flush", e.fs.Flush("/f", fh), -fuse.EIO)
		e.fs.Release("/f", fh)

		if got, _ := e.remote(t, "f"); got != "theirs" {
			t.Fatalf("remote = %q, want the other writer's version", got)
		}
	})
}

// T-204: O_TRUNC, O_APPEND and a sparse write past EOF.
func TestOpenFlagsAndSparseWrite(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		e.put(t, "f", "existing", nil)

		fh := e.open(t, "/f", fuse.O_WRONLY|fuse.O_TRUNC)
		check(t, "Write", e.fs.Write("/f", []byte("new"), 0, fh), 3)
		check(t, "Flush", e.fs.Flush("/f", fh), 0)
		e.fs.Release("/f", fh)

		if got, _ := e.remote(t, "f"); got != "new" {
			t.Fatalf("after O_TRUNC: %q", got)
		}

		fh = e.open(t, "/f", fuse.O_WRONLY|fuse.O_APPEND)
		check(t, "Write", e.fs.Write("/f", []byte("+tail"), 0, fh), 5) // offset ignored
		check(t, "Flush", e.fs.Flush("/f", fh), 0)
		e.fs.Release("/f", fh)

		if got, _ := e.remote(t, "f"); got != "new+tail" {
			t.Fatalf("after O_APPEND: %q", got)
		}

		fh = e.open(t, "/f", fuse.O_RDWR)
		check(t, "Write", e.fs.Write("/f", []byte("!"), 12, fh), 1)
		check(t, "Flush", e.fs.Flush("/f", fh), 0)
		e.fs.Release("/f", fh)

		if got, _ := e.remote(t, "f"); got != "new+tail\x00\x00\x00\x00!" {
			t.Fatalf("after sparse write: %q", got)
		}
	})
}

// T-205: concurrent writers on different files (run with -race).
func TestConcurrentWritesDifferentFiles(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		const n = 8

		var wg sync.WaitGroup

		errs := make(chan string, n)

		for i := range n {
			wg.Add(1)

			go func() {
				defer wg.Done()

				path := fmt.Sprintf("/w%d", i)
				data := []byte(strings.Repeat(fmt.Sprint(i), 1000))

				rc, fh := e.fs.Create(path, fuse.O_WRONLY, 0o644)
				if rc != 0 {
					errs <- fmt.Sprintf("Create(%s) = %d", path, rc)
					return
				}

				for off := 0; off < len(data); off += 100 {
					e.fs.Write(path, data[off:off+100], int64(off), fh)
				}

				if rc := e.fs.Flush(path, fh); rc != 0 {
					errs <- fmt.Sprintf("Flush(%s) = %d", path, rc)
				}

				e.fs.Release(path, fh)
			}()
		}

		wg.Wait()
		close(errs)

		for msg := range errs {
			t.Error(msg)
		}

		for i := range n {
			want := strings.Repeat(fmt.Sprint(i), 1000)
			if got, _ := e.remote(t, fmt.Sprintf("w%d", i)); got != want {
				t.Errorf("w%d: got %d bytes", i, len(got))
			}
		}
	})
}

// T-205: two handles on the same file share staged data.
func TestHandlesShareStagedData(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		e.put(t, "f", "aaaa", nil)

		w := e.open(t, "/f", fuse.O_WRONLY)
		r := e.open(t, "/f", fuse.O_RDONLY)

		check(t, "Write", e.fs.Write("/f", []byte("bb"), 0, w), 2)

		if got := e.readHandle(t, "/f", r); got != "bbaa" {
			t.Fatalf("reader sees %q, want the staged write", got)
		}

		check(t, "Flush(reader)", e.fs.Flush("/f", r), 0)
		e.fs.Release("/f", r)
		e.fs.Release("/f", w)

		if got, _ := e.remote(t, "f"); got != "bbaa" {
			t.Fatalf("remote = %q", got)
		}
	})
}

// T-206: files over --max-staged-bytes are refused.
func TestMaxStagedBytes(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		rc, fh := e.fs.Create("/big", fuse.O_WRONLY, 0o644)
		check(t, "Create", rc, 0)
		check(t, "Write within limit", e.fs.Write("/big", make([]byte, 1024), 0, fh), 1024)
		check(t, "Write past limit", e.fs.Write("/big", []byte("x"), 1024, fh), -fuse.EFBIG)
		check(t, "Truncate past limit", e.fs.Truncate("/big", 2048, fh), -fuse.EFBIG)
		e.fs.Release("/big", fh)

		e.put(t, "huge", strings.Repeat("h", 2048), nil)
		fh = e.open(t, "/huge", fuse.O_WRONLY)
		check(t, "Write to existing oversized file", e.fs.Write("/huge", []byte("x"), 0, fh), -fuse.EFBIG)
		e.fs.Release("/huge", fh)

		if got := e.readFile(t, "/huge"); len(got) != 2048 {
			t.Fatalf("oversized file still readable: got %d bytes", len(got))
		}
	}, func(c *Config) { c.MaxStagedBytes = 1024 })
}

// T-207: chmod and utimens persist as metadata without rewriting data.
func TestChmodUtimensChown(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		before := e.put(t, "f", "data", nil)
		e.put(t, "d/", "", nil)
		e.put(t, "implicit/x", "x", nil)

		check(t, "Chmod", e.fs.Chmod("/f", 0o600), 0)

		mtime := time.Date(2020, 1, 2, 3, 4, 5, 6, time.UTC)
		ts := []fuse.Timespec{fuse.NewTimespec(mtime), fuse.NewTimespec(mtime)}
		check(t, "Utimens", e.fs.Utimens("/f", ts), 0)

		omit := []fuse.Timespec{fuse.NewTimespec(time.Now()), {Nsec: fuse.UTIME_OMIT}}
		check(t, "Utimens(mtime omitted)", e.fs.Utimens("/f", omit), 0)

		after, _ := e.raw.Stat(context.Background(), "f")
		if after.Generation != before.Generation {
			t.Fatal("metadata change rewrote the object")
		}

		st := e.stat(t, "/f")
		if st.Mode&0o7777 != 0o600 || !st.Mtim.Time().Equal(mtime) {
			t.Fatalf("stat after chmod/utimens: mode %o mtime %v", st.Mode, st.Mtim.Time())
		}

		check(t, "Chmod(dir)", e.fs.Chmod("/d", 0o700), 0)

		if st := e.stat(t, "/d"); st.Mode&0o7777 != 0o700 {
			t.Fatalf("dir mode = %o", st.Mode)
		}

		check(t, "Chmod(implicit dir)", e.fs.Chmod("/implicit", 0o700), 0)
		check(t, "Chown(same ids)", e.fs.Chown("/f", 1000, ^uint32(0)), 0)
		check(t, "Chown(other uid)", e.fs.Chown("/f", 0, 0), -fuse.EPERM)
	})
}

// T-207: chmod on a file that is open and dirty is applied at upload.
func TestChmodOnDirtyFile(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		rc, fh := e.fs.Create("/f", fuse.O_WRONLY, 0o644)
		check(t, "Create", rc, 0)
		check(t, "Chmod", e.fs.Chmod("/f", 0o604), 0)
		check(t, "Flush", e.fs.Flush("/f", fh), 0)
		e.fs.Release("/f", fh)

		a, _ := e.raw.Stat(context.Background(), "f")
		if a.Metadata[metaMode] != "604" {
			t.Fatalf("mode metadata = %q", a.Metadata[metaMode])
		}
	})
}

// T-208: a new file is visible locally before its first flush.
func TestNewFileVisibleBeforeFlush(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		e.put(t, "dir/", "", nil)

		rc, fh := e.fs.Create("/dir/pending", fuse.O_WRONLY, 0o644)
		check(t, "Create", rc, 0)
		check(t, "Write", e.fs.Write("/dir/pending", []byte("abc"), 0, fh), 3)

		wantNames(t, e.ls(t, "/dir"), "pending")

		if st := e.stat(t, "/dir/pending"); st.Size != 3 {
			t.Fatalf("pending size = %d", st.Size)
		}

		e.fs.Release("/dir/pending", fh) // release without flush still saves
		if got, _ := e.remote(t, "dir/pending"); got != "abc" {
			t.Fatalf("remote = %q", got)
		}
	})
}

func TestMknod(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		check(t, "Mknod(fifo)", e.fs.Mknod("/fifo", fuse.S_IFIFO|0o644, 0), -fuse.EPERM)
		check(t, "Mknod(regular)", e.fs.Mknod("/reg", fuse.S_IFREG|0o600, 0), 0)

		if st := e.stat(t, "/reg"); st.Mode != fuse.S_IFREG|0o600 || st.Size != 0 {
			t.Fatalf("mknod result: %+v", st)
		}

		check(t, "Mknod(existing)", e.fs.Mknod("/reg", fuse.S_IFREG|0o600, 0), -fuse.EEXIST)
	})
}

func TestFlushFailureReturnsEIO(t *testing.T) {
	mb := newMemBucket()
	e := newEnv(t, mb)

	rc, fh := e.fs.Create("/f", fuse.O_WRONLY, 0o644)
	check(t, "Create", rc, 0)

	mb.failWrite = func(string) error { return errors.New("503 backend unavailable") }
	check(t, "Flush during outage", e.fs.Flush("/f", fh), -fuse.EIO)

	mb.failWrite = nil
	check(t, "Flush after recovery", e.fs.Flush("/f", fh), 0)
	e.fs.Release("/f", fh)

	if !e.exists(t, "f") {
		t.Fatal("retry after outage did not save the file")
	}
}

// ---- M3: namespace operations ----

// T-305: mkdir creates a placeholder; mkdir of an existing name fails.
func TestMkdir(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		e.put(t, "file", "x", nil)
		e.put(t, "imp/x", "x", nil)

		check(t, "Mkdir", e.fs.Mkdir("/d", 0o750), 0)

		if st := e.stat(t, "/d"); !isDir(st) || st.Mode&0o7777 != 0o750 {
			t.Fatalf("new dir stat: %o", st.Mode)
		}

		if !e.exists(t, "d/") {
			t.Fatal("placeholder d/ not created")
		}

		check(t, "Mkdir(existing dir)", e.fs.Mkdir("/d", 0o755), -fuse.EEXIST)
		check(t, "Mkdir(existing file)", e.fs.Mkdir("/file", 0o755), -fuse.EEXIST)
		check(t, "Mkdir(implicit dir)", e.fs.Mkdir("/imp", 0o755), -fuse.EEXIST)
		check(t, "Mkdir(nested)", e.fs.Mkdir("/d/sub", 0o755), 0)
		wantNames(t, e.ls(t, "/d"), "sub")
	})
}

// T-301
func TestRmdir(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		e.put(t, "empty/", "", nil)
		e.put(t, "full/", "", nil)
		e.put(t, "full/x", "x", nil)
		e.put(t, "imp/x", "x", nil)
		e.put(t, "nested/", "", nil)
		e.put(t, "nested/sub/y", "y", nil)

		check(t, "Rmdir(empty)", e.fs.Rmdir("/empty"), 0)

		if e.exists(t, "empty/") {
			t.Fatal("placeholder not deleted")
		}

		check(t, "Rmdir(non-empty)", e.fs.Rmdir("/full"), -fuse.ENOTEMPTY)
		check(t, "Rmdir(implicit with child)", e.fs.Rmdir("/imp"), -fuse.ENOTEMPTY)
		check(t, "Rmdir(only a subdir)", e.fs.Rmdir("/nested"), -fuse.ENOTEMPTY)
		check(t, "Rmdir(missing)", e.fs.Rmdir("/missing"), -fuse.ENOENT)

		check(t, "Mkdir", e.fs.Mkdir("/p", 0o755), 0)
		rc, fh := e.fs.Create("/p/pending", fuse.O_WRONLY, 0o644)
		check(t, "Create", rc, 0)
		check(t, "Rmdir(pending local child)", e.fs.Rmdir("/p"), -fuse.ENOTEMPTY)
		e.fs.Release("/p/pending", fh)
	})
}

// T-302: rename a file over another; open handles follow the file.
func TestRenameFile(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		e.put(t, "src", "source data", map[string]string{metaMode: "600"})
		e.put(t, "dst", "old dst", nil)

		fh := e.open(t, "/src", fuse.O_RDONLY)

		check(t, "Rename", e.fs.Rename("/src", "/dst"), 0)

		if e.exists(t, "src") {
			t.Fatal("source still exists")
		}

		if got, _ := e.remote(t, "dst"); got != "source data" {
			t.Fatalf("dst = %q", got)
		}

		if st := e.stat(t, "/dst"); st.Mode&0o7777 != 0o600 {
			t.Fatalf("rename lost the mode: %o", st.Mode)
		}

		if got := e.readHandle(t, "/dst", fh); got != "source data" {
			t.Fatalf("handle opened before rename reads %q", got)
		}

		e.fs.Release("/dst", fh)

		check(t, "Rename(missing)", e.fs.Rename("/nope", "/x"), -fuse.ENOENT)
	})
}

// T-302: renaming a file that hasn't been flushed yet uploads it first.
func TestRenamePendingFile(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		rc, fh := e.fs.Create("/tmp.part", fuse.O_WRONLY, 0o644)
		check(t, "Create", rc, 0)
		check(t, "Write", e.fs.Write("/tmp.part", []byte("payload"), 0, fh), 7)
		check(t, "Rename", e.fs.Rename("/tmp.part", "/final"), 0)
		check(t, "Write after rename", e.fs.Write("/final", []byte("!"), 7, fh), 1)
		check(t, "Flush", e.fs.Flush("/final", fh), 0)
		e.fs.Release("/final", fh)

		if e.exists(t, "tmp.part") {
			t.Fatal("old name still exists")
		}

		if got, _ := e.remote(t, "final"); got != "payload!" {
			t.Fatalf("final = %q", got)
		}
	})
}

// T-303: directory rename moves every object; over the limit it's EXDEV.
func TestRenameDir(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		e.put(t, "a/", "", map[string]string{metaMode: "700"})
		e.put(t, "a/f1", "1", nil)
		e.put(t, "a/sub/f2", "2", nil)

		check(t, "Rename(dir)", e.fs.Rename("/a", "/b"), 0)

		for _, gone := range []string{"a/", "a/f1", "a/sub/f2"} {
			if e.exists(t, gone) {
				t.Fatalf("%s still exists", gone)
			}
		}

		if got, _ := e.remote(t, "b/sub/f2"); got != "2" {
			t.Fatalf("b/sub/f2 = %q", got)
		}

		if st := e.stat(t, "/b"); !isDir(st) || st.Mode&0o7777 != 0o700 {
			t.Fatalf("/b stat: %o", st.Mode)
		}

		check(t, "Rename(into itself)", e.fs.Rename("/b", "/b/sub/x"), -fuse.EINVAL)

		e.put(t, "c/f", "x", nil)
		check(t, "Rename(dir over non-empty dir)", e.fs.Rename("/b", "/c"), -fuse.ENOTEMPTY)
		check(t, "Rename(dir over file)", e.fs.Rename("/b", "/c/f"), -fuse.ENOTDIR)
		check(t, "Rename(file over dir)", e.fs.Rename("/c/f", "/b"), -fuse.EISDIR)
	})
}

func TestRenameDirOverLimit(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		for i := range 4 {
			e.put(t, fmt.Sprintf("big/%d", i), "x", nil)
		}

		check(t, "Rename(big dir)", e.fs.Rename("/big", "/moved"), -fuse.EXDEV)

		if !e.exists(t, "big/3") || e.exists(t, "moved/3") {
			t.Fatal("EXDEV rename changed the bucket")
		}
	}, func(c *Config) { c.RenameDirLimit = 3 })
}

// T-303: open files inside a renamed directory follow it.
func TestRenameDirWithOpenFile(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		e.put(t, "a/", "", nil)

		rc, fh := e.fs.Create("/a/open", fuse.O_WRONLY, 0o644)
		check(t, "Create", rc, 0)
		check(t, "Write", e.fs.Write("/a/open", []byte("v1"), 0, fh), 2)
		check(t, "Rename", e.fs.Rename("/a", "/b"), 0)
		check(t, "Write after rename", e.fs.Write("/b/open", []byte("v2"), 0, fh), 2)
		check(t, "Flush", e.fs.Flush("/b/open", fh), 0)
		e.fs.Release("/b/open", fh)

		if e.exists(t, "a/open") {
			t.Fatal("file recreated under the old directory")
		}

		if got, _ := e.remote(t, "b/open"); got != "v2" {
			t.Fatalf("b/open = %q", got)
		}
	})
}

// T-304: unlink while open; staged data stays readable; no resurrection.
func TestUnlinkWhileOpen(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		rc, fh := e.fs.Create("/f", fuse.O_RDWR, 0o644)
		check(t, "Create", rc, 0)
		check(t, "Write", e.fs.Write("/f", []byte("still here"), 0, fh), 10)
		check(t, "Flush", e.fs.Flush("/f", fh), 0)
		check(t, "Unlink", e.fs.Unlink("/f"), 0)

		if got := e.readHandle(t, "/f", fh); got != "still here" {
			t.Fatalf("read after unlink = %q", got)
		}

		check(t, "Write after unlink", e.fs.Write("/f", []byte("X"), 0, fh), 1)
		check(t, "Flush after unlink", e.fs.Flush("/f", fh), 0)
		e.fs.Release("/f", fh)

		if e.exists(t, "f") {
			t.Fatal("unlinked file was recreated by flush")
		}

		var st fuse.Stat_t
		check(t, "Getattr", e.fs.Getattr("/f", &st, noFH), -fuse.ENOENT)
		check(t, "Unlink(missing)", e.fs.Unlink("/f"), -fuse.ENOENT)
	})
}

func TestUnlinkPendingFile(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		rc, fh := e.fs.Create("/p", fuse.O_WRONLY, 0o644)
		check(t, "Create", rc, 0)
		check(t, "Unlink(never uploaded)", e.fs.Unlink("/p"), 0)
		e.fs.Release("/p", fh)

		if e.exists(t, "p") {
			t.Fatal("pending file was uploaded after unlink")
		}
	})
}

// ---- M4: caching and read streams ----

// T-401: within the TTL a repeated lookup costs no GCS calls; local changes
// invalidate the cache.
func TestStatCache(t *testing.T) {
	e := newEnv(t, newMemBucket(), func(c *Config) { c.StatTTL = time.Minute })
	e.put(t, "f", "x", nil)

	e.stat(t, "/f")
	before := e.calls.Total()
	e.stat(t, "/f")

	if d := e.calls.Total() - before; d != 0 {
		t.Fatalf("cached Getattr made %d GCS calls", d)
	}

	check(t, "Chmod", e.fs.Chmod("/f", 0o600), 0)

	if st := e.stat(t, "/f"); st.Mode&0o7777 != 0o600 {
		t.Fatalf("stale cache after chmod: %o", st.Mode)
	}

	check(t, "Unlink", e.fs.Unlink("/f"), 0)

	var st fuse.Stat_t
	check(t, "Getattr after unlink", e.fs.Getattr("/f", &st, noFH), -fuse.ENOENT)
}

// T-402: misses are cached too.
func TestNegativeCache(t *testing.T) {
	e := newEnv(t, newMemBucket(), func(c *Config) { c.StatTTL = time.Minute })

	var st fuse.Stat_t
	check(t, "Getattr", e.fs.Getattr("/.git", &st, noFH), -fuse.ENOENT)

	before := e.calls.Total()
	check(t, "Getattr", e.fs.Getattr("/.git", &st, noFH), -fuse.ENOENT)

	if d := e.calls.Total() - before; d != 0 {
		t.Fatalf("cached miss made %d GCS calls", d)
	}

	e.writeFile(t, "/.git", "now it exists")

	if e.stat(t, "/.git").Size != 13 {
		t.Fatal("negative entry not invalidated by create")
	}
}

// T-401: TTL expiry; another client's change becomes visible.
func TestCacheExpiry(t *testing.T) {
	e := newEnv(t, newMemBucket(), func(c *Config) { c.StatTTL = time.Minute })

	now := time.Now()
	e.fs.cache.now = func() time.Time { return now }

	e.put(t, "f", "short", nil)
	e.stat(t, "/f")
	e.put(t, "f", "much longer", nil)

	if e.stat(t, "/f").Size != 5 {
		t.Fatal("expected the cached size within the TTL")
	}

	now = now.Add(2 * time.Minute)

	if e.stat(t, "/f").Size != 11 {
		t.Fatal("expected the new size after the TTL")
	}
}

// EXP-001: expired entries go away once the cache grows, even when their
// names are never looked up again (as after a big `ls -R`).
func TestCacheSweepsExpiredEntries(t *testing.T) {
	now := time.Now()
	c := newStatCache(time.Second, func() time.Time { return now })

	for i := range 2 * minSweep {
		c.putIfUnchanged(fmt.Sprintf("old/%d", i), entry{exists: true}, c.snapshot())
	}

	now = now.Add(time.Minute)

	for i := range 2 * minSweep {
		c.putIfUnchanged(fmt.Sprintf("new/%d", i), entry{exists: true}, c.snapshot())
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	for k := range c.m {
		if strings.HasPrefix(k, "old/") {
			t.Fatalf("expired entry %q still cached (%d entries)", k, len(c.m))
		}
	}

	if len(c.m) != 2*minSweep {
		t.Fatalf("%d entries cached, want the %d live ones", len(c.m), 2*minSweep)
	}
}

// T-403: ls -l pattern; readdir fills the cache so per-file lookups are free.
// Subdirectories still cost one lookup (a stat plus a one-entry list): a
// listing can't reveal a placeholder's stored mode (review finding #5).
func TestReaddirFillsCache(t *testing.T) {
	e := newEnv(t, newMemBucket(), func(c *Config) { c.StatTTL = time.Minute })

	for i := range 20 {
		e.put(t, fmt.Sprintf("dir/f%02d", i), "x", nil)
	}

	e.put(t, "dir/sub/x", "x", nil)

	names := e.ls(t, "/dir")
	before := e.calls.Total()

	for _, n := range names {
		if n != "sub" {
			e.stat(t, "/dir/"+n)
		}
	}

	if d := e.calls.Total() - before; d != 0 {
		t.Fatalf("stat of %d listed files made %d GCS calls", len(names)-1, d)
	}

	e.stat(t, "/dir/sub")
	e.stat(t, "/dir/sub")

	if d := e.calls.Total() - before; d != 2 {
		t.Fatalf("stat of a listed subdirectory (twice) made %d GCS calls, want 2", d)
	}
}

// T-404: a sequential read of a multi-chunk file uses one GCS read with
// streams on, and one per FUSE read with streams off.
func TestReadStreamsReduceRequests(t *testing.T) {
	data := strings.Repeat("s", 40*4096)

	for _, tc := range []struct {
		streams bool
		want    int64
	}{{true, 1}, {false, 40}} {
		e := newEnv(t, newMemBucket(), func(c *Config) { c.ReadStreams = tc.streams })
		e.put(t, "f", data, nil)

		if got := e.readFile(t, "/f"); got != data {
			t.Fatal("content mismatch")
		}

		if got := e.calls.Counts()[opRead]; got != tc.want {
			t.Fatalf("streams=%v: %d GCS reads, want %d", tc.streams, got, tc.want)
		}
	}
}

// ---- regressions for the independent review ----

// failDelete makes every Delete fail, as during an outage or a retention lock.
type failDelete struct{ *memBucket }

func (failDelete) Delete(context.Context, string, int64) error {
	return errors.New("503 backend unavailable")
}

// pauseStat lets a test run code between a Stat fetching its result and the
// filesystem using it.
type pauseStat struct {
	*memBucket
	fetched, resume chan struct{}
}

func (p *pauseStat) Stat(ctx context.Context, name string) (ObjAttrs, error) {
	a, err := p.memBucket.Stat(ctx, name)
	if p.fetched != nil {
		p.fetched <- struct{}{}
		<-p.resume
	}

	return a, err
}

// Review #1: a local reader opening the file must not rebase a writer's
// generation, or the writer silently overwrites another client's version.
func TestReaderOpenDoesNotBypassConflictCheck(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		e.put(t, "f", "v1", nil)

		w := e.open(t, "/f", fuse.O_WRONLY)
		e.put(t, "f", "theirs", nil)

		r := e.open(t, "/f", fuse.O_RDONLY)
		if got := e.readHandle(t, "/f", r); got != "theirs" {
			t.Fatalf("reader opened after the change reads %q, want the new version", got)
		}

		check(t, "Write by the older writer", e.fs.Write("/f", []byte("mine"), 0, w), -fuse.EIO)
		e.fs.Release("/f", r)
		e.fs.Release("/f", w)

		if got, _ := e.remote(t, "f"); got != "theirs" {
			t.Fatalf("remote = %q, want the other writer's version", got)
		}
	})
}

// Review #1: with no writer open, a new open (reader or writer) does see the
// latest version.
func TestNewWriterRebasesWhenOnlyReadersOpen(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		e.put(t, "f", "v1", nil)

		r := e.open(t, "/f", fuse.O_RDONLY)
		e.put(t, "f", "v2", nil)

		w := e.open(t, "/f", fuse.O_WRONLY|fuse.O_APPEND)
		check(t, "Write", e.fs.Write("/f", []byte("+"), 0, w), 2-1)
		check(t, "Flush", e.fs.Flush("/f", w), 0)
		e.fs.Release("/f", w)
		e.fs.Release("/f", r)

		if got, _ := e.remote(t, "f"); got != "v2+" {
			t.Fatalf("remote = %q, want v2+", got)
		}
	})
}

// Review #2: a failed unlink must leave the file fully linked.
func TestFailedUnlinkKeepsFile(t *testing.T) {
	mb := newMemBucket()
	e := newEnv(t, failDelete{mb})
	e.put(t, "f", "v1", nil)

	fh := e.open(t, "/f", fuse.O_RDWR)
	check(t, "Write", e.fs.Write("/f", []byte("v2"), 0, fh), 2)
	check(t, "Unlink during outage", e.fs.Unlink("/f"), -fuse.EIO)
	check(t, "Flush", e.fs.Flush("/f", fh), 0)
	e.fs.Release("/f", fh)

	if got, _ := e.remote(t, "f"); got != "v2" {
		t.Fatalf("remote = %q, want v2 (the unlink failed, so the write must be saved)", got)
	}

	rc, fh := e.fs.Create("/pending", fuse.O_WRONLY, 0o644)
	check(t, "Create", rc, 0)
	check(t, "Unlink(local-only) during outage", e.fs.Unlink("/pending"), 0)
	e.fs.Release("/pending", fh)
}

// Review #3a: a directory with an unflushed child is not empty.
func TestRenameDirOverDirWithPendingChild(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		e.put(t, "a/x", "AAA", nil)
		e.put(t, "b/", "", nil)

		rc, fh := e.fs.Create("/b/x", fuse.O_WRONLY, 0o644)
		check(t, "Create", rc, 0)
		check(t, "Write", e.fs.Write("/b/x", []byte("BBB"), 0, fh), 3)
		check(t, "Rename(/a, /b)", e.fs.Rename("/a", "/b"), -fuse.ENOTEMPTY)
		check(t, "Flush", e.fs.Flush("/b/x", fh), 0)
		e.fs.Release("/b/x", fh)

		if got, _ := e.remote(t, "b/x"); got != "BBB" {
			t.Fatalf("b/x = %q, want BBB", got)
		}
	})
}

// Review #3b: an unflushed file at the destination is a file.
func TestRenameOverPendingFile(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		e.put(t, "dir/f", "x", nil)
		e.put(t, "src", "SRC", nil)

		rc, fh := e.fs.Create("/pending", fuse.O_WRONLY, 0o644)
		check(t, "Create", rc, 0)
		check(t, "Write", e.fs.Write("/pending", []byte("PENDING"), 0, fh), 7)
		check(t, "Rename(dir over pending file)", e.fs.Rename("/dir", "/pending"), -fuse.ENOTDIR)

		// A file renamed over it replaces it, and the replaced file's later
		// flush must not resurrect it.
		check(t, "Rename(file over pending file)", e.fs.Rename("/src", "/pending"), 0)
		check(t, "Write to replaced file", e.fs.Write("/pending", []byte("LATE"), 0, fh), 4)
		check(t, "Flush replaced file", e.fs.Flush("/pending", fh), 0)
		e.fs.Release("/pending", fh)

		if got, _ := e.remote(t, "pending"); got != "SRC" {
			t.Fatalf("pending = %q, want SRC", got)
		}
	})
}

// Review #4: a refused directory rename leaves the destination alone.
func TestRefusedDirRenameKeepsDestination(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		for i := range 4 {
			e.put(t, fmt.Sprintf("big/%d", i), "x", nil)
		}

		e.put(t, "empty/", "", map[string]string{metaMode: "700"})

		check(t, "Rename(big, empty)", e.fs.Rename("/big", "/empty"), -fuse.EXDEV)

		if !e.exists(t, "empty/") {
			t.Fatal("EXDEV rename deleted the destination directory")
		}
	}, func(c *Config) { c.RenameDirLimit = 3 })
}

// Review #4: renaming onto an empty explicit directory works and keeps the
// source directory's mode.
func TestRenameDirOntoEmptyDir(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		e.put(t, "src/", "", map[string]string{metaMode: "711"})
		e.put(t, "src/f", "f", nil)
		e.put(t, "dst/", "", map[string]string{metaMode: "700"})

		check(t, "Rename", e.fs.Rename("/src", "/dst"), 0)

		if st := e.stat(t, "/dst"); st.Mode&0o7777 != 0o711 {
			t.Fatalf("dst mode = %o, want the source's 0711", st.Mode)
		}

		if got, _ := e.remote(t, "dst/f"); got != "f" || e.exists(t, "src/") {
			t.Fatalf("dst/f = %q, src/ exists = %v", got, e.exists(t, "src/"))
		}
	})
}

// Review #5: listing a directory must not hide its children's stored modes.
func TestReaddirKeepsPlaceholderMode(t *testing.T) {
	e := newEnv(t, newMemBucket(), func(c *Config) { c.StatTTL = time.Minute })
	e.put(t, "d1/", "", map[string]string{metaMode: "700"})
	e.put(t, "d1/f", "x", nil)

	var dirStat *fuse.Stat_t

	e.fs.Readdir("/", func(name string, st *fuse.Stat_t, _ int64) bool {
		if name == "d1" {
			dirStat = st
		}

		return true
	}, 0, noFH)

	if dirStat != nil && dirStat.Mode&0o7777 != 0o700 {
		t.Fatalf("readdirplus reported d1 as %o; must be 0700 or no attributes", dirStat.Mode)
	}

	if st := e.stat(t, "/d1"); st.Mode&0o7777 != 0o700 {
		t.Fatalf("stat after ls = %o, want 0700", st.Mode)
	}
}

// Review #6: with implicit dirs off, readdir agrees with getattr.
func TestReaddirImplicitDirsDisabled(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		e.put(t, "implicit/x", "x", nil)
		e.put(t, "c", "file", nil)
		e.put(t, "c/inner", "x", nil)
		e.put(t, "explicit/", "", nil)

		var kinds []string

		e.fs.Readdir("/", func(name string, st *fuse.Stat_t, _ int64) bool {
			if name != "." && name != ".." {
				var gst fuse.Stat_t
				if rc := e.fs.Getattr("/"+name, &gst, noFH); rc != 0 {
					t.Errorf("readdir listed %q but Getattr = %d", name, rc)
				}

				if st != nil && st.Mode&fuse.S_IFMT != gst.Mode&fuse.S_IFMT {
					t.Errorf("%q: readdir type %o, getattr type %o", name, st.Mode&fuse.S_IFMT, gst.Mode&fuse.S_IFMT)
				}

				kinds = append(kinds, fmt.Sprintf("%s:%v", name, isDir(gst)))
			}

			return true
		}, 0, noFH)

		wantNames(t, kinds, "c:false", "explicit:true")
	}, func(c *Config) { c.ImplicitDirs = false })
}

// Review #7: a lookup that raced with a local delete must not re-cache the
// deleted file.
func TestLookupRacingUnlinkDoesNotCacheStaleEntry(t *testing.T) {
	ps := &pauseStat{memBucket: newMemBucket()}
	e := newEnv(t, ps, func(c *Config) { c.StatTTL = time.Minute })
	e.put(t, "f", "x", nil)

	ps.fetched, ps.resume = make(chan struct{}), make(chan struct{})
	done := make(chan int)

	go func() {
		var st fuse.Stat_t
		done <- e.fs.Getattr("/f", &st, noFH)
	}()

	<-ps.fetched // the lookup has seen "f exists"
	ps.fetched = nil
	check(t, "Unlink", e.fs.Unlink("/f"), 0)
	close(ps.resume)
	<-done

	var st fuse.Stat_t
	check(t, "Getattr after unlink", e.fs.Getattr("/f", &st, noFH), -fuse.ENOENT)
}

// Review #10: when the source delete fails, the copy is still visible.
func TestRenameWithFailedDeleteShowsDestination(t *testing.T) {
	e := newEnv(t, failDelete{newMemBucket()}, func(c *Config) { c.StatTTL = time.Minute })
	e.put(t, "src", "data", nil)

	var st fuse.Stat_t
	check(t, "Getattr(dst) before", e.fs.Getattr("/dst", &st, noFH), -fuse.ENOENT)
	check(t, "Rename with failing delete", e.fs.Rename("/src", "/dst"), -fuse.EIO)
	check(t, "Getattr(dst) after", e.fs.Getattr("/dst", &st, noFH), 0)
}

// T-404 under concurrency: the kernel sends readahead reads on one handle at
// the same time. Content must stay correct (run with -race). The request
// count under real readahead is checked through a mount (measure step).
func TestConcurrentReadsOnOneHandle(t *testing.T) {
	forEachEnv(t, func(t *testing.T, e *testEnv) {
		data := make([]byte, 64*4096)
		for i := range data {
			data[i] = byte(i * 7)
		}

		e.put(t, "f", string(data), nil)

		fh := e.open(t, "/f", fuse.O_RDONLY)
		defer e.fs.Release("/f", fh)

		var wg sync.WaitGroup

		out := make([]byte, len(data))

		for i := range 64 {
			wg.Add(1)

			go func() {
				defer wg.Done()

				if n := e.fs.Read("/f", out[i*4096:(i+1)*4096], int64(i*4096), fh); n != 4096 {
					t.Errorf("read %d = %d", i, n)
				}
			}()
		}

		wg.Wait()

		if !bytes.Equal(out, data) {
			t.Fatal("content mismatch under concurrent reads")
		}
	})
}

// deleteAfterCopy stands in for another client that deletes a rename's source
// right after the server-side copy.
type deleteAfterCopy struct{ *memBucket }

func (d deleteAfterCopy) Copy(ctx context.Context, src string, srcGen int64, dst string, dstCond int64) (ObjAttrs, error) {
	a, err := d.memBucket.Copy(ctx, src, srcGen, dst, dstCond)
	if err == nil {
		_ = d.memBucket.Delete(ctx, src, condNone)
	}

	return a, err
}

// EXP-003: if the source is already gone when rename deletes it, the rename
// has happened. Report success and repoint open handles.
func TestRenameSourceDeletedAfterCopy(t *testing.T) {
	e := newEnv(t, deleteAfterCopy{newMemBucket()})
	e.put(t, "a", "data", nil)

	fh := e.open(t, "/a", fuse.O_RDONLY)
	check(t, "Rename", e.fs.Rename("/a", "/b"), 0)

	if e.exists(t, "a") {
		t.Fatal("source still exists")
	}

	if got, _ := e.remote(t, "b"); got != "data" {
		t.Fatalf("destination = %q, want %q", got, "data")
	}

	if h := e.fs.handle(fh); h.name != "b" {
		t.Fatalf("open handle still points at %q", h.name)
	}

	if got := e.readHandle(t, "/b", fh); got != "data" {
		t.Fatalf("read through the moved handle = %q", got)
	}

	e.fs.Release("/b", fh)
}
