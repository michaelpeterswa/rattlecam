package publish

import (
	"context"
	"errors"
	"fmt"
)

// ErrStore marks a failure to reach the object store, as distinct from a
// failure to write the local copy.
//
// The two deserve different treatment. A local write failure means the frame
// did not land at all. A store failure means it landed on disk but did not
// reach the bucket viewers read from — the feed goes stale rather than broken,
// and the daemon must keep going rather than treat the cycle as lost.
var ErrStore = errors.New("object store")

// ObjectStore is somewhere frames are published besides the local disk.
//
// Implementations must make a completed Put visible atomically: a reader has to
// see either the previous object or the new one, never a prefix of it. That is
// the same guarantee the local temp-file-and-rename gives, and it is why a
// half-uploaded frame must never be readable.
type ObjectStore interface {
	// Put writes data at name, replacing anything already there.
	Put(ctx context.Context, name string, data []byte, opts PutOptions) error

	// Close releases any client resources.
	Close() error
}

// PutOptions carries the metadata that has to travel with an object.
type PutOptions struct {
	ContentType string

	// CacheControl matters more than it looks. A bucket's default is
	// public, max-age=3600, so a stable latest.jpg URL would serve a
	// hour-old frame from cache long after a newer one landed.
	CacheControl string
}

// storeErr wraps a store failure so callers can tell it apart with errors.Is.
func storeErr(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("publish: %w: %s: %v", ErrStore, op, err)
}
