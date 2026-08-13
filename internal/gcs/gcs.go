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
	"time"

	"cloud.google.com/go/storage"

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

// Close releases the underlying client.
func (c *Client) Close() error {
	if c.client == nil {
		return nil
	}
	return c.client.Close()
}
