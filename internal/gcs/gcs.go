// Package gcs publishes frames to a Google Cloud Storage bucket.
//
// The camera sits on a radio tower with a finite uplink, and the number of
// people watching is not something the tower should have to care about. Pushing
// each frame once to a bucket and letting readers pull from there keeps the
// upstream cost fixed at one upload per frame however many viewers there are.
package gcs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"

	"github.com/michaelpeterswa/rattlecam/internal/publish"
)

// Client writes objects into one bucket.
type Client struct {
	client *storage.Client
	bucket *storage.BucketHandle
	name   string

	// timeout bounds a single upload. Without it a stalled connection would
	// hold up the publishing loop indefinitely.
	timeout time.Duration
}

// New connects using Application Default Credentials — the metadata server on
// GCP, or GOOGLE_APPLICATION_CREDENTIALS pointing at a key file elsewhere,
// which is what an on-premise host uses.
func New(ctx context.Context, bucket string) (*Client, error) {
	if bucket == "" {
		return nil, errors.New("gcs: bucket is required")
	}

	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcs: %w", err)
	}

	return &Client{
		client:  client,
		bucket:  client.Bucket(bucket),
		name:    bucket,
		timeout: 60 * time.Second,
	}, nil
}

// Bucket reports which bucket this client writes to.
func (c *Client) Bucket() string { return c.name }

// Put uploads data, replacing whatever was there.
//
// A GCS object becomes visible only once the write completes, so a reader sees
// either the previous frame or the new one and never a partial upload. That is
// the same guarantee the local temp-file-and-rename provides, which is why this
// needs no staging object of its own.
func (c *Client) Put(ctx context.Context, name string, data []byte, opts publish.PutOptions) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	w := c.bucket.Object(name).NewWriter(ctx)
	w.ContentType = opts.ContentType
	w.CacheControl = opts.CacheControl
	// Send it in one request. These are a couple of megabytes; chunking buys
	// nothing and only adds round trips.
	w.ChunkSize = 0

	if _, err := w.Write(data); err != nil {
		// Close still has to run, or the underlying request leaks. Its error is
		// uninteresting once the write has already failed.
		_ = w.Close()
		return fmt.Errorf("gcs: write %s: %w", name, err)
	}
	// The upload is only committed by Close, so its error is the one that says
	// whether the object actually landed.
	if err := w.Close(); err != nil {
		return fmt.Errorf("gcs: commit %s: %w", name, err)
	}
	return nil
}

// listTimeout bounds a listing, which is a different shape of operation from an
// upload: many small round trips rather than one large one, and a month of
// archived days is several thousand names.
const listTimeout = 5 * time.Minute

// List returns the names of every object under a prefix, sorted.
//
// Sorted because the archive encodes time in the name — zero-padded HHMMSS
// under a zero-padded date — so lexical order is chronological order, and a
// caller assembling a timelapse needs no other index.
func (c *Client) List(ctx context.Context, prefix string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()

	q := &storage.Query{Prefix: prefix}
	// Ask only for the name. A full listing carries every attribute of every
	// object, and on a few thousand frames that is most of the response body
	// for something the caller discards.
	if err := q.SetAttrSelection([]string{"Name"}); err != nil {
		return nil, fmt.Errorf("gcs: list %s: %w", prefix, err)
	}

	var names []string
	it := c.bucket.Objects(ctx, q)

	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("gcs: list %s: %w", prefix, err)
		}
		names = append(names, attrs.Name)
	}

	sort.Strings(names)
	return names, nil
}

// Close releases the underlying client.
func (c *Client) Close() error {
	if c.client == nil {
		return nil
	}
	return c.client.Close()
}

// Object is a fetched object plus the metadata needed to serve it on.
type Object struct {
	Data        []byte
	Generation  int64
	Updated     time.Time
	ContentType string
}

// Generation reports the current generation of an object without fetching it.
//
// This is the cheap half of serving: a metadata read costs a fraction of a
// download, so a gateway can check whether anything changed far more often than
// it transfers two megabytes.
func (c *Client) Generation(ctx context.Context, name string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	attrs, err := c.bucket.Object(name).Attrs(ctx)
	if err != nil {
		return 0, fmt.Errorf("gcs: attrs %s: %w", name, err)
	}
	return attrs.Generation, nil
}

// Get downloads an object.
func (c *Client) Get(ctx context.Context, name string) (Object, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	r, err := c.bucket.Object(name).NewReader(ctx)
	if err != nil {
		return Object{}, fmt.Errorf("gcs: open %s: %w", name, err)
	}
	defer r.Close() //nolint:errcheck // read-only

	data, err := io.ReadAll(r)
	if err != nil {
		return Object{}, fmt.Errorf("gcs: read %s: %w", name, err)
	}

	return Object{
		Data:        data,
		Generation:  r.Attrs.Generation,
		Updated:     r.Attrs.LastModified,
		ContentType: r.Attrs.ContentType,
	}, nil
}

// ErrNotFound reports an object that is not in the bucket.
func IsNotFound(err error) bool { return errors.Is(err, storage.ErrObjectNotExist) }
