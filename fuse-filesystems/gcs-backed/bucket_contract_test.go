package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/fsouza/fake-gcs-server/fakestorage"
)

const testBucket = "lab"

// backend is a Bucket implementation the tests run against.
type backend struct {
	name string
	new  func(t *testing.T) Bucket
	// copyDeletePreconds is false for fake-gcs-server, which ignores
	// generation preconditions on copy (rewrite) and delete.
	copyDeletePreconds bool
}

var backends = []backend{
	{"fake-gcs-server", func(t *testing.T) Bucket { return newFakeBucket(t) }, false},
	{"memBucket", func(*testing.T) Bucket { return newMemBucket() }, true},
}

// newFakeBucket returns the real adapter (gcsBucket + official client) talking
// to an in-process fake-gcs-server. No listener, no credentials.
func newFakeBucket(t *testing.T) *gcsBucket {
	t.Helper()

	srv, err := fakestorage.NewServerWithOptions(fakestorage.Options{NoListener: true})
	if err != nil {
		t.Fatalf("fake server: %v", err)
	}

	srv.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: testBucket})
	t.Cleanup(srv.Stop)

	return newGCSBucket(srv.Client().Bucket(testBucket))
}

func forEachBackend(t *testing.T, fn func(t *testing.T, bk backend)) {
	for _, bk := range backends {
		t.Run(bk.name, func(t *testing.T) { fn(t, bk) })
	}
}

func mustWrite(t *testing.T, b Bucket, name string, cond int64, data string, meta map[string]string) ObjAttrs {
	t.Helper()

	a, err := b.Write(context.Background(), name, cond, meta, strings.NewReader(data))
	if err != nil {
		t.Fatalf("Write(%q): %v", name, err)
	}

	return a
}

func readString(t *testing.T, b Bucket, name string, gen, off, length int64) (string, error) {
	t.Helper()

	r, err := b.NewReader(context.Background(), name, gen, off, length)
	if err != nil {
		return "", err
	}
	defer r.Close()

	data, err := io.ReadAll(r)

	return string(data), err
}

// T0.3: generation preconditions on writes.
func TestContractWritePreconditions(t *testing.T) {
	forEachBackend(t, func(t *testing.T, bk backend) {
		b := bk.new(t)
		ctx := context.Background()

		a1 := mustWrite(t, b, "f", condNotExist, "one", nil)
		if a1.Generation <= 0 || a1.Size != 3 {
			t.Fatalf("attrs after create = %+v", a1)
		}

		if _, err := b.Write(ctx, "f", condNotExist, nil, strings.NewReader("x")); !errors.Is(err, errPrecondition) {
			t.Fatalf("create over existing: err = %v, want errPrecondition", err)
		}

		a2 := mustWrite(t, b, "f", a1.Generation, "two", nil)
		if a2.Generation == a1.Generation {
			t.Fatal("generation did not change on overwrite")
		}

		if _, err := b.Write(ctx, "f", a1.Generation, nil, strings.NewReader("x")); !errors.Is(err, errPrecondition) {
			t.Fatalf("overwrite with stale generation: err = %v, want errPrecondition", err)
		}

		if _, err := b.Stat(ctx, "missing"); !errors.Is(err, errNotFound) {
			t.Fatalf("Stat(missing): err = %v, want errNotFound", err)
		}
	})
}

// T0.3: delimiter listings; a limit of 1 returns the placeholder first.
func TestContractListing(t *testing.T) {
	forEachBackend(t, func(t *testing.T, bk backend) {
		b := bk.new(t)
		ctx := context.Background()

		for _, n := range []string{"a/", "a/x", "a/y/z", "a/b0", "b", "c/d"} {
			mustWrite(t, b, n, condNone, "", nil)
		}

		res, err := b.List(ctx, "a/", "/", 0)
		if err != nil {
			t.Fatal(err)
		}

		var names []string
		for _, o := range res.Objects {
			names = append(names, o.Name)
		}

		if !slices.Equal(names, []string{"a/", "a/b0", "a/x"}) || !slices.Equal(res.Prefixes, []string{"a/y/"}) {
			t.Fatalf("List(a/) objects=%v prefixes=%v", names, res.Prefixes)
		}

		root, err := b.List(ctx, "", "/", 0)
		if err != nil {
			t.Fatal(err)
		}

		if len(root.Objects) != 1 || root.Objects[0].Name != "b" || !slices.Equal(root.Prefixes, []string{"a/", "c/"}) {
			t.Fatalf("List(root) = %+v", root)
		}

		one, err := b.List(ctx, "a/", "/", 1)
		if err != nil {
			t.Fatal(err)
		}

		if len(one.Prefixes) != 0 || len(one.Objects) != 1 || one.Objects[0].Name != "a/" {
			t.Fatalf("List(a/, limit 1) = %+v, want the placeholder only", one)
		}

		implicit, err := b.List(ctx, "c/", "/", 1)
		if err != nil {
			t.Fatal(err)
		}

		if len(implicit.Objects) != 1 || implicit.Objects[0].Name != "c/d" {
			t.Fatalf("List(c/, limit 1) = %+v", implicit)
		}

		flat, err := b.List(ctx, "a/", "", 0)
		if err != nil {
			t.Fatal(err)
		}

		if len(flat.Objects) != 4 || len(flat.Prefixes) != 0 {
			t.Fatalf("flat List(a/) = %+v, want 4 objects", flat)
		}
	})
}

// T0.3: range reads, and reads pinned to a generation that has been replaced.
func TestContractReads(t *testing.T) {
	forEachBackend(t, func(t *testing.T, bk backend) {
		b := bk.new(t)

		a1 := mustWrite(t, b, "f", condNotExist, "0123456789", nil)

		if got, err := readString(t, b, "f", a1.Generation, 2, 3); err != nil || got != "234" {
			t.Fatalf("range read = %q, %v", got, err)
		}

		if got, err := readString(t, b, "f", 0, 7, -1); err != nil || got != "789" {
			t.Fatalf("read to end = %q, %v", got, err)
		}

		mustWrite(t, b, "f", a1.Generation, "new", nil)

		if _, err := readString(t, b, "f", a1.Generation, 0, -1); !errors.Is(err, errNotFound) {
			t.Fatalf("read of replaced generation: err = %v, want errNotFound", err)
		}
	})
}

// T0.3: copy keeps data and metadata; delete; metadata update merges keys and
// keeps the generation. Copy/delete preconditions only where supported.
func TestContractCopyDeleteUpdate(t *testing.T) {
	forEachBackend(t, func(t *testing.T, bk backend) {
		b := bk.new(t)
		ctx := context.Background()

		src := mustWrite(t, b, "src", condNotExist, "data", map[string]string{metaMode: "640"})

		dst, err := b.Copy(ctx, "src", src.Generation, "dst", condNotExist)
		if err != nil {
			t.Fatalf("Copy: %v", err)
		}

		if dst.Metadata[metaMode] != "640" || dst.Size != 4 {
			t.Fatalf("copied attrs = %+v", dst)
		}

		if bk.copyDeletePreconds {
			if _, err := b.Copy(ctx, "src", src.Generation, "dst", condNotExist); !errors.Is(err, errPrecondition) {
				t.Fatalf("copy over existing: err = %v, want errPrecondition", err)
			}

			if err := b.Delete(ctx, "src", src.Generation+1); !errors.Is(err, errPrecondition) {
				t.Fatalf("delete with wrong generation: err = %v, want errPrecondition", err)
			}
		}

		if err := b.Delete(ctx, "src", src.Generation); err != nil {
			t.Fatalf("Delete: %v", err)
		}

		if err := b.Delete(ctx, "src", condNone); !errors.Is(err, errNotFound) {
			t.Fatalf("delete missing: err = %v, want errNotFound", err)
		}

		up, err := b.UpdateMetadata(ctx, "dst", map[string]string{metaMtime: "123"})
		if err != nil {
			t.Fatalf("UpdateMetadata: %v", err)
		}

		if up.Generation != dst.Generation || up.Metadata[metaMode] != "640" || up.Metadata[metaMtime] != "123" {
			t.Fatalf("after update: %+v (generation before %d)", up, dst.Generation)
		}

		if got, _ := readString(t, b, "dst", 0, 0, -1); got != "data" {
			t.Fatalf("dst content = %q", got)
		}

		big := bytes.Repeat([]byte("z"), 3<<20)
		if _, err := b.Write(ctx, "big", condNotExist, nil, bytes.NewReader(big)); err != nil {
			t.Fatalf("3 MiB write: %v", err)
		}

		if got, _ := readString(t, b, "big", 0, 3<<20-2, -1); got != "zz" {
			t.Fatalf("tail of 3 MiB object = %q", got)
		}
	})
}
