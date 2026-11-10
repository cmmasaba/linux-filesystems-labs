package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"time"
)

// ObjAttrs is the subset of GCS object attributes the filesystem uses.
type ObjAttrs struct {
	Name       string
	Size       int64
	Generation int64
	Updated    time.Time
	Created    time.Time
	Metadata   map[string]string
}

// ListResult holds one directory-style listing. With a delimiter, Prefixes are
// the "subdirectories" (each ends in the delimiter) and Objects are the direct
// children, including a placeholder object equal to the listing prefix.
type ListResult struct {
	Objects  []ObjAttrs
	Prefixes []string
}

// Generation preconditions for writes, copies and deletes. Any value > 0 means
// "only if the object's current generation equals this value".
const (
	condNone     int64 = -1 // no precondition
	condNotExist int64 = 0  // only if the object does not exist
)

var (
	errNotFound     = errors.New("object not found")
	errPrecondition = errors.New("precondition failed")
	errPermission   = errors.New("permission denied")
)

// Bucket is the narrow view of GCS the filesystem needs. gcsBucket implements
// it with cloud.google.com/go/storage. Keeping it small keeps GCS details in
// one file and lets countingBucket measure how many calls an operation costs.
type Bucket interface {
	Stat(ctx context.Context, name string) (ObjAttrs, error)
	// List returns objects under prefix. With delim "/" it collapses deeper
	// names into Prefixes. limit <= 0 means no limit.
	List(ctx context.Context, prefix, delim string, limit int) (ListResult, error)
	// NewReader reads length bytes (-1 = to the end) from offset off of
	// generation gen (0 = latest).
	NewReader(ctx context.Context, name string, gen, off, length int64) (io.ReadCloser, error)
	// Write replaces the object's contents and custom metadata.
	Write(ctx context.Context, name string, cond int64, meta map[string]string, r io.Reader) (ObjAttrs, error)
	// Copy copies generation srcGen (0 = latest) of src to dst, keeping metadata.
	Copy(ctx context.Context, src string, srcGen int64, dst string, dstCond int64) (ObjAttrs, error)
	Delete(ctx context.Context, name string, cond int64) error
	// UpdateMetadata merges meta into the object's custom metadata without
	// touching its data. The generation does not change.
	UpdateMetadata(ctx context.Context, name string, meta map[string]string) (ObjAttrs, error)
}

// Operation names used for counting and debug logging.
const (
	opStat   = "stat"
	opList   = "list"
	opRead   = "read"
	opWrite  = "write"
	opCopy   = "copy"
	opDelete = "delete"
	opUpdate = "update"
)

var allOps = []string{opStat, opList, opRead, opWrite, opCopy, opDelete, opUpdate}

// countingBucket wraps a Bucket, counts calls per operation and logs each call
// at debug level. It never logs object contents.
type countingBucket struct {
	b      Bucket
	counts map[string]*atomic.Int64
}

func newCountingBucket(b Bucket) *countingBucket {
	c := &countingBucket{b: b, counts: map[string]*atomic.Int64{}}
	for _, op := range allOps {
		c.counts[op] = &atomic.Int64{}
	}

	return c
}

// Counts returns a snapshot of the call counters.
func (c *countingBucket) Counts() map[string]int64 {
	out := make(map[string]int64, len(c.counts))
	for op, n := range c.counts {
		out[op] = n.Load()
	}

	return out
}

// Total returns the total number of calls made.
func (c *countingBucket) Total() int64 {
	var t int64
	for _, n := range c.counts {
		t += n.Load()
	}

	return t
}

func (c *countingBucket) record(op, name string, start time.Time, err error) {
	c.counts[op].Add(1)
	slog.Debug("gcs", "op", op, "object", name, "latency", time.Since(start), "err", err)
}

func (c *countingBucket) Stat(ctx context.Context, name string) (a ObjAttrs, err error) {
	defer func(t time.Time) { c.record(opStat, name, t, err) }(time.Now())
	return c.b.Stat(ctx, name)
}

func (c *countingBucket) List(ctx context.Context, prefix, delim string, limit int) (r ListResult, err error) {
	defer func(t time.Time) { c.record(opList, prefix, t, err) }(time.Now())
	return c.b.List(ctx, prefix, delim, limit)
}

func (c *countingBucket) NewReader(ctx context.Context, name string, gen, off, length int64) (rc io.ReadCloser, err error) {
	defer func(t time.Time) { c.record(opRead, name, t, err) }(time.Now())
	return c.b.NewReader(ctx, name, gen, off, length)
}

func (c *countingBucket) Write(ctx context.Context, name string, cond int64, meta map[string]string, r io.Reader) (a ObjAttrs, err error) {
	defer func(t time.Time) { c.record(opWrite, name, t, err) }(time.Now())
	return c.b.Write(ctx, name, cond, meta, r)
}

func (c *countingBucket) Copy(ctx context.Context, src string, srcGen int64, dst string, dstCond int64) (a ObjAttrs, err error) {
	defer func(t time.Time) { c.record(opCopy, src+" -> "+dst, t, err) }(time.Now())
	return c.b.Copy(ctx, src, srcGen, dst, dstCond)
}

func (c *countingBucket) Delete(ctx context.Context, name string, cond int64) (err error) {
	defer func(t time.Time) { c.record(opDelete, name, t, err) }(time.Now())
	return c.b.Delete(ctx, name, cond)
}

func (c *countingBucket) UpdateMetadata(ctx context.Context, name string, meta map[string]string) (a ObjAttrs, err error) {
	defer func(t time.Time) { c.record(opUpdate, name, t, err) }(time.Now())
	return c.b.UpdateMetadata(ctx, name, meta)
}
