package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

type gcsBucket struct {
	bkt *storage.BucketHandle
}

func newGCSBucket(bkt *storage.BucketHandle) *gcsBucket {
	return &gcsBucket{bkt: bkt}
}

var listAttrs = []string{"Name", "Size", "Generation", "Updated", "Created", "Metadata"}

func (b *gcsBucket) object(name string, cond int64) *storage.ObjectHandle {
	o := b.bkt.Object(name)

	switch {
	case cond == condNotExist:
		o = o.If(storage.Conditions{DoesNotExist: true})
	case cond > 0:
		o = o.If(storage.Conditions{GenerationMatch: cond})
	}

	return o
}

func toObjAttrs(a *storage.ObjectAttrs) ObjAttrs {
	return ObjAttrs{
		Name:       a.Name,
		Size:       a.Size,
		Generation: a.Generation,
		Updated:    a.Updated,
		Created:    a.Created,
		Metadata:   a.Metadata,
	}
}

func convErr(err error) error {
	if err == nil {
		return nil
	}

	if errors.Is(err, storage.ErrObjectNotExist) {
		return fmt.Errorf("%w: %w", errNotFound, err)
	}

	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		switch gerr.Code {
		case http.StatusNotFound:
			return fmt.Errorf("%w: %w", errNotFound, err)
		case http.StatusPreconditionFailed:
			return fmt.Errorf("%w: %w", errPrecondition, err)
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf("%w: %w", errPermission, err)
		}
	}

	return err
}

func (b *gcsBucket) Stat(ctx context.Context, name string) (ObjAttrs, error) {
	a, err := b.bkt.Object(name).Attrs(ctx)
	if err != nil {
		return ObjAttrs{}, convErr(err)
	}

	return toObjAttrs(a), nil
}

func (b *gcsBucket) List(ctx context.Context, prefix, delim string, limit int) (ListResult, error) {
	q := &storage.Query{Prefix: prefix, Delimiter: delim}
	if err := q.SetAttrSelection(listAttrs); err != nil {
		return ListResult{}, err
	}

	it := b.bkt.Objects(ctx, q)
	if limit > 0 {
		it.PageInfo().MaxSize = limit
	}

	var res ListResult

	for n := 0; limit <= 0 || n < limit; n++ {
		a, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}

		if err != nil {
			return ListResult{}, convErr(err)
		}

		if a.Prefix != "" {
			res.Prefixes = append(res.Prefixes, a.Prefix)
		} else {
			res.Objects = append(res.Objects, toObjAttrs(a))
		}
	}

	return res, nil
}

func (b *gcsBucket) NewReader(ctx context.Context, name string, gen, off, length int64) (io.ReadCloser, error) {
	o := b.bkt.Object(name)
	if gen > 0 {
		o = o.Generation(gen)
	}

	// Read the stored bytes, so offsets and sizes line up with ObjAttrs.Size
	// even for objects uploaded with Content-Encoding: gzip.
	r, err := o.ReadCompressed(true).NewRangeReader(ctx, off, length)
	if err != nil {
		return nil, convErr(err)
	}

	return r, nil
}

func (b *gcsBucket) Write(ctx context.Context, name string, cond int64, meta map[string]string, r io.Reader) (ObjAttrs, error) {
	w := b.object(name, cond).NewWriter(ctx)
	w.Metadata = meta

	if _, err := io.Copy(w, r); err != nil {
		_ = w.Close()
		return ObjAttrs{}, convErr(err)
	}

	if err := w.Close(); err != nil {
		return ObjAttrs{}, convErr(err)
	}

	return toObjAttrs(w.Attrs()), nil
}

func (b *gcsBucket) Copy(ctx context.Context, src string, srcGen int64, dst string, dstCond int64) (ObjAttrs, error) {
	s := b.bkt.Object(src)
	if srcGen > 0 {
		s = s.Generation(srcGen)
	}

	a, err := b.object(dst, dstCond).CopierFrom(s).Run(ctx)
	if err != nil {
		return ObjAttrs{}, convErr(err)
	}

	return toObjAttrs(a), nil
}

func (b *gcsBucket) Delete(ctx context.Context, name string, cond int64) error {
	return convErr(b.object(name, cond).Delete(ctx))
}

func (b *gcsBucket) UpdateMetadata(ctx context.Context, name string, meta map[string]string) (ObjAttrs, error) {
	a, err := b.bkt.Object(name).Update(ctx, storage.ObjectAttrsToUpdate{Metadata: meta})
	if err != nil {
		return ObjAttrs{}, convErr(err)
	}

	return toObjAttrs(a), nil
}
