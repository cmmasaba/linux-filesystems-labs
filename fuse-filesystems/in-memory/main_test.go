package main

import (
	"bytes"
	"testing"

	"github.com/winfsp/cgofuse/fuse"
)

// noFH is the "no file handle" value cgofuse passes for path-based calls.
const noFH = ^uint64(0)

// addNode attaches a node directly under parent. Tests build the tree by hand
// because makeNode calls fuse.Getcontext, which is only valid on a FUSE
// request thread.
func addNode(fs *FileSystem, parent *node, name string, mode uint32) *node {
	fs.inode++
	n := newNode(0, fs.inode, mode, 0, 0)
	parent.child[name] = n

	return n
}

func readAll(t *testing.T, fs *FileSystem, path string, size int) []byte {
	t.Helper()

	buf := make([]byte, size)
	n := fs.Read(path, buf, 0, noFH)
	if n < 0 {
		t.Fatalf("Read(%s) = %d", path, n)
	}

	return buf[:n]
}

// B1: shrinking and then growing a file inside the same allocation unit used
// to expose the bytes that the shrink should have discarded.
func TestTruncateShrinkThenGrowZeroFills(t *testing.T) {
	fs := NewFileSystem()
	addNode(fs, fs.root, "f", fuse.S_IFREG|0o644)

	if n := fs.Write("/f", bytes.Repeat([]byte{'x'}, 50), 0, noFH); n != 50 {
		t.Fatalf("Write = %d, want 50", n)
	}

	if rc := fs.Truncate("/f", 10, noFH); rc != 0 {
		t.Fatalf("Truncate(10) = %d", rc)
	}

	if rc := fs.Truncate("/f", 100, noFH); rc != 0 {
		t.Fatalf("Truncate(100) = %d", rc)
	}

	want := append(bytes.Repeat([]byte{'x'}, 10), make([]byte, 90)...)
	if got := readAll(t, fs, "/f", 200); !bytes.Equal(got, want) {
		t.Fatalf("content after shrink+grow:\n got %q\nwant %q", got, want)
	}
}

// B1, write variant: a sparse write past EOF after a shrink must leave a hole
// of zeros, not old data.
func TestSparseWriteAfterShrinkZeroFills(t *testing.T) {
	fs := NewFileSystem()
	addNode(fs, fs.root, "f", fuse.S_IFREG|0o644)

	fs.Write("/f", bytes.Repeat([]byte{'x'}, 50), 0, noFH)
	fs.Truncate("/f", 10, noFH)

	if n := fs.Write("/f", []byte("y"), 60, noFH); n != 1 {
		t.Fatalf("Write = %d, want 1", n)
	}

	want := append(bytes.Repeat([]byte{'x'}, 10), make([]byte, 50)...)
	want = append(want, 'y')

	if got := readAll(t, fs, "/f", 200); !bytes.Equal(got, want) {
		t.Fatalf("content after sparse write:\n got %q\nwant %q", got, want)
	}
}

// B2: moving a directory into its own subtree must fail and leave the tree intact.
func TestRenameDirectoryIntoItselfFails(t *testing.T) {
	fs := NewFileSystem()
	a := addNode(fs, fs.root, "a", fuse.S_IFDIR|0o755)

	if rc := fs.Rename("/a", "/a/b"); rc != -fuse.EINVAL {
		t.Fatalf("Rename(/a, /a/b) = %d, want %d (EINVAL)", rc, -fuse.EINVAL)
	}

	if fs.root.child["a"] != a {
		t.Fatal("/a was detached from the root")
	}

	if _, ok := a.child["b"]; ok {
		t.Fatal("/a/b was created, forming a cycle")
	}
}

// B3: link count starts at 1 and does not wrap around on delete.
func TestNlinkDoesNotUnderflow(t *testing.T) {
	fs := NewFileSystem()
	f := addNode(fs, fs.root, "f", fuse.S_IFREG|0o644)

	if f.stat.Nlink != 1 {
		t.Fatalf("Nlink = %d, want 1", f.stat.Nlink)
	}

	if rc := fs.Unlink("/f"); rc != 0 {
		t.Fatalf("Unlink = %d", rc)
	}

	if f.stat.Nlink != 0 {
		t.Fatalf("Nlink after unlink = %d, want 0", f.stat.Nlink)
	}
}
