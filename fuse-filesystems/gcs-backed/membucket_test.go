package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

// memBucket is an in-memory Bucket with GCS semantics: monotonically
// increasing generations, generation preconditions on every mutation,
// non-versioned reads, and delimiter listings in lexicographic order.
//
// It exists because fake-gcs-server v1.56.1 ignores preconditions on copy
// and delete (found in the M0 spike). bucket_contract_test.go keeps the two
// fakes in agreement on everything else.
type memBucket struct {
	mu   sync.Mutex
	objs map[string]*memObj
	gen  int64
	now  func() time.Time

	// failWrite, if set, is consulted before each Write (fault injection).
	failWrite func(name string) error
}

type memObj struct {
	data  []byte
	attrs ObjAttrs
}

func newMemBucket() *memBucket {
	return &memBucket{objs: map[string]*memObj{}, gen: 1000, now: time.Now}
}

func cloneAttrs(a ObjAttrs) ObjAttrs {
	a.Metadata = maps.Clone(a.Metadata)
	return a
}

func (m *memBucket) checkCond(name string, cond int64) error {
	o := m.objs[name]

	switch {
	case cond == condNotExist && o != nil:
		return errPrecondition
	case cond > 0 && (o == nil || o.attrs.Generation != cond):
		return errPrecondition
	}

	return nil
}

func (m *memBucket) Stat(_ context.Context, name string) (ObjAttrs, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	o := m.objs[name]
	if o == nil {
		return ObjAttrs{}, errNotFound
	}

	return cloneAttrs(o.attrs), nil
}

func (m *memBucket) List(_ context.Context, prefix, delim string, limit int) (ListResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	names := slices.Sorted(maps.Keys(m.objs))

	var res ListResult

	seen := map[string]bool{}
	count := 0

	for _, n := range names {
		if !strings.HasPrefix(n, prefix) {
			continue
		}

		if limit > 0 && count >= limit {
			break
		}

		rest := n[len(prefix):]
		if i := strings.Index(rest, delim); delim != "" && i >= 0 {
			p := prefix + rest[:i+len(delim)]
			if !seen[p] {
				seen[p] = true
				res.Prefixes = append(res.Prefixes, p)
				count++
			}

			continue
		}

		res.Objects = append(res.Objects, cloneAttrs(m.objs[n].attrs))
		count++
	}

	return res, nil
}

func (m *memBucket) NewReader(_ context.Context, name string, gen, off, length int64) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	o := m.objs[name]
	if o == nil || (gen > 0 && o.attrs.Generation != gen) {
		return nil, errNotFound
	}

	size := int64(len(o.data))
	if off > size {
		return nil, errors.New("416 requested range not satisfiable")
	}

	end := size
	if length >= 0 {
		end = min(off+length, size)
	}

	return io.NopCloser(bytes.NewReader(bytes.Clone(o.data[off:end]))), nil
}

func (m *memBucket) Write(_ context.Context, name string, cond int64, meta map[string]string, r io.Reader) (ObjAttrs, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return ObjAttrs{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.failWrite != nil {
		if err := m.failWrite(name); err != nil {
			return ObjAttrs{}, err
		}
	}

	if err := m.checkCond(name, cond); err != nil {
		return ObjAttrs{}, err
	}

	return m.store(name, data, meta), nil
}

// store writes an object unconditionally. m.mu must be held.
func (m *memBucket) store(name string, data []byte, meta map[string]string) ObjAttrs {
	m.gen++
	now := m.now()
	o := &memObj{data: data, attrs: ObjAttrs{
		Name: name, Size: int64(len(data)), Generation: m.gen,
		Updated: now, Created: now, Metadata: maps.Clone(meta),
	}}
	m.objs[name] = o

	return cloneAttrs(o.attrs)
}

func (m *memBucket) Copy(_ context.Context, src string, srcGen int64, dst string, dstCond int64) (ObjAttrs, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	s := m.objs[src]
	if s == nil || (srcGen > 0 && s.attrs.Generation != srcGen) {
		return ObjAttrs{}, errNotFound
	}

	if err := m.checkCond(dst, dstCond); err != nil {
		return ObjAttrs{}, err
	}

	return m.store(dst, bytes.Clone(s.data), s.attrs.Metadata), nil
}

func (m *memBucket) Delete(_ context.Context, name string, cond int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.objs[name] == nil {
		return errNotFound
	}

	if err := m.checkCond(name, cond); err != nil {
		return err
	}

	delete(m.objs, name)

	return nil
}

func (m *memBucket) UpdateMetadata(_ context.Context, name string, meta map[string]string) (ObjAttrs, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	o := m.objs[name]
	if o == nil {
		return ObjAttrs{}, errNotFound
	}

	if o.attrs.Metadata == nil {
		o.attrs.Metadata = map[string]string{}
	}

	for k, v := range meta {
		if v == "" {
			delete(o.attrs.Metadata, k)
		} else {
			o.attrs.Metadata[k] = v
		}
	}

	o.attrs.Updated = m.now()

	return cloneAttrs(o.attrs), nil
}
