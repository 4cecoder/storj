// Copyright (C) 2021 Storj Labs, Inc.
// See LICENSE for copying information.

package ulfs

import (
	"context"
	"strings"

	"github.com/zeebo/errs"

	"storj.io/storj/cmd/uplinkng/ulloc"
	"storj.io/uplink"
)

// Remote implements something close to a filesystem but backed by an uplink project.
type Remote struct {
	project *uplink.Project
}

// NewRemote returns something close to a filesystem and returns objects using the project.
func NewRemote(project *uplink.Project) *Remote {
	return &Remote{
		project: project,
	}
}

// Close releases any resources that the Remote contains.
func (r *Remote) Close() error {
	return r.project.Close()
}

// Open returns a ReadHandle for the object identified by a given bucket and key.
func (r *Remote) Open(ctx context.Context, bucket, key string) (ReadHandle, error) {
	fh, err := r.project.DownloadObject(ctx, bucket, key, nil)
	if err != nil {
		return nil, errs.Wrap(err)
	}
	return newUplinkReadHandle(bucket, fh), nil
}

// Create returns a WriteHandle for the object identified by a given bucket and key.
func (r *Remote) Create(ctx context.Context, bucket, key string) (WriteHandle, error) {
	fh, err := r.project.UploadObject(ctx, bucket, key, nil)
	if err != nil {
		return nil, errs.Wrap(err)
	}
	return newUplinkWriteHandle(fh), nil
}

// Remove deletes the object at the provided key and bucket.
func (r *Remote) Remove(ctx context.Context, bucket, key string, opts *RemoveOptions) error {
	if !opts.isPending() {
		_, err := r.project.DeleteObject(ctx, bucket, key)
		if err != nil {
			return errs.Wrap(err)
		}
		return nil
	}

	// TODO: we may need a dedicated endpoint for deleting pending object streams
	list := r.project.ListUploads(ctx, bucket, &uplink.ListUploadsOptions{Prefix: key})

	// TODO: modify when we can have several pending objects for the same object key
	if list.Next() {
		err := r.project.AbortUpload(ctx, bucket, key, list.Item().UploadID)
		if err != nil {
			return errs.Wrap(err)
		}
	}
	if err := list.Err(); err != nil {
		return errs.Wrap(err)
	}
	return nil
}

// List lists all of the objects in some bucket that begin with the given prefix.
func (r *Remote) List(ctx context.Context, bucket, prefix string, opts *ListOptions) ObjectIterator {
	parentPrefix := ""
	if idx := strings.LastIndexByte(prefix, '/'); idx >= 0 {
		parentPrefix = prefix[:idx+1]
	}

	trim := ulloc.NewRemote(bucket, "")
	if !opts.isRecursive() {
		trim = ulloc.NewRemote(bucket, parentPrefix)
	}

	var iter ObjectIterator
	if opts.isPending() {
		iter = newUplinkUploadIterator(
			bucket,
			r.project.ListUploads(ctx, bucket, &uplink.ListUploadsOptions{
				Prefix:    parentPrefix,
				Recursive: opts.Recursive,
				System:    true,
			}),
		)
	} else {
		iter = newUplinkObjectIterator(
			bucket,
			r.project.ListObjects(ctx, bucket, &uplink.ListObjectsOptions{
				Prefix:    parentPrefix,
				Recursive: opts.Recursive,
				System:    true,
			}),
		)
	}

	return &filteredObjectIterator{
		trim:   trim,
		filter: ulloc.NewRemote(bucket, prefix),
		iter:   iter,
	}
}

// uplinkObjectIterator implements objectIterator for *uplink.ObjectIterator.
type uplinkObjectIterator struct {
	bucket string
	iter   *uplink.ObjectIterator
}

// newUplinkObjectIterator constructs an *uplinkObjectIterator from an *uplink.ObjectIterator.
func newUplinkObjectIterator(bucket string, iter *uplink.ObjectIterator) *uplinkObjectIterator {
	return &uplinkObjectIterator{
		bucket: bucket,
		iter:   iter,
	}
}

func (u *uplinkObjectIterator) Next() bool { return u.iter.Next() }
func (u *uplinkObjectIterator) Err() error { return u.iter.Err() }
func (u *uplinkObjectIterator) Item() ObjectInfo {
	return uplinkObjectToObjectInfo(u.bucket, u.iter.Item())
}

// uplinkUploadIterator implements objectIterator for *multipart.UploadIterators.
type uplinkUploadIterator struct {
	bucket string
	iter   *uplink.UploadIterator
}

// newUplinkUploadIterator constructs a *uplinkUploadIterator from a *uplink.UploadIterator.
func newUplinkUploadIterator(bucket string, iter *uplink.UploadIterator) *uplinkUploadIterator {
	return &uplinkUploadIterator{
		bucket: bucket,
		iter:   iter,
	}
}

func (u *uplinkUploadIterator) Next() bool { return u.iter.Next() }
func (u *uplinkUploadIterator) Err() error { return u.iter.Err() }
func (u *uplinkUploadIterator) Item() ObjectInfo {
	return uplinkUploadInfoToObjectInfo(u.bucket, u.iter.Item())
}
