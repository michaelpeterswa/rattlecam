// Package protect pulls stills from a UniFi Protect camera through the console's
// integration API.
package protect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"net/http"
	"strings"
	"time"

	_ "image/jpeg" // snapshots come back as JPEG
)

const apiBase = "/proxy/protect/integration/v1"

// Client talks to one camera on one console.
type Client struct {
	host     string
	apiKey   string
	cameraID string
	http     *http.Client
}

// New builds a client. certSHA256 is the hex SHA-256 of the console's leaf
// certificate; see the README for how to capture it.
//
// UniFi consoles ship a self-signed certificate, so the choice is between
// pinning that certificate and not verifying at all. Leaving certSHA256 empty
// takes the second option, which is fine on a bench and less fine for something
// feeding a newsroom — a device swap on the LAN could then silently substitute
// the image.
func New(host, apiKey, cameraID, certSHA256 string) (*Client, error) {
	if host == "" {
		return nil, errors.New("protect: host is required")
	}
	if apiKey == "" {
		return nil, errors.New("protect: api key is required")
	}
	if cameraID == "" {
		return nil, errors.New("protect: camera id is required")
	}

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}

	if certSHA256 != "" {
		want, err := parseFingerprint(certSHA256)
		if err != nil {
			return nil, err
		}
		// Chain verification against the system roots can never succeed for a
		// self-signed console, so it is disabled and replaced wholesale by the
		// pin. This is strictly stronger than the unset case, not weaker.
		tlsCfg.InsecureSkipVerify = true
		tlsCfg.VerifyPeerCertificate = pinnedVerifier(want)
	} else {
		tlsCfg.InsecureSkipVerify = true
	}

	return &Client{
		host:     strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://"), "/"),
		apiKey:   apiKey,
		cameraID: cameraID,
		http: &http.Client{
			Timeout:   20 * time.Second,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
	}, nil
}

// parseFingerprint accepts the colon-separated form openssl prints as well as
// bare hex, in any case.
func parseFingerprint(s string) ([]byte, error) {
	clean := strings.ToLower(s)
	for _, sep := range []string{":", " ", "-"} {
		clean = strings.ReplaceAll(clean, sep, "")
	}
	if i := strings.Index(clean, "="); i >= 0 { // "SHA256 Fingerprint=AB:CD:..."
		clean = clean[i+1:]
	}
	b, err := hex.DecodeString(clean)
	if err != nil {
		return nil, fmt.Errorf("protect: cert fingerprint: %w", err)
	}
	if len(b) != sha256.Size {
		return nil, fmt.Errorf("protect: cert fingerprint: want %d bytes of sha256, got %d", sha256.Size, len(b))
	}
	return b, nil
}

// pinnedVerifier fails closed: any certificate that is not the pinned one is
// rejected, including an otherwise perfectly valid publicly-trusted one.
func pinnedVerifier(want []byte) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("protect: server presented no certificate")
		}
		got := sha256.Sum256(rawCerts[0])
		if subtle.ConstantTimeCompare(got[:], want) != 1 {
			return fmt.Errorf("protect: certificate pin mismatch: got %s, want %s",
				hex.EncodeToString(got[:]), hex.EncodeToString(want))
		}
		return nil
	}
}

// Still is one snapshot, kept in both forms.
//
// Raw matters as much as Image. The camera hands over a finished JPEG, so
// anything published unmodified should be those bytes rather than a decode and
// re-encode of them: the round trip is a generational loss and roughly doubles
// the size for no benefit. Only a frame that has actually been drawn on needs
// encoding again.
type Still struct {
	// Raw is exactly what the camera sent.
	Raw []byte
	// Image is Raw decoded, for compositing.
	Image image.Image
}

// Snapshot fetches a still and decodes it, keeping the original bytes.
func (c *Client) Snapshot(ctx context.Context, highQuality bool) (*Still, error) {
	// highQuality is a string enum in the API; %t emits exactly "true"/"false".
	// The default is false, which returns a downscaled frame.
	url := fmt.Sprintf("https://%s%s/cameras/%s/snapshot?highQuality=%t",
		c.host, apiBase, c.cameraID, highQuality)

	resp, err := c.do(ctx, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body

	if resp.StatusCode != http.StatusOK {
		return nil, statusError("snapshot", resp)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "image/") {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return nil, fmt.Errorf("protect: snapshot returned %s, not an image: %s",
			ct, strings.TrimSpace(string(body)))
	}

	// A 4K frame is around a megabyte; the cap is generous enough not to
	// truncate one but still bounds a misbehaving endpoint.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("protect: read snapshot: %w", err)
	}

	// Decoding is also the check that Raw is a real image, so the bytes handed
	// on for publishing are known good rather than merely well-labelled.
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("protect: decode snapshot: %w", err)
	}

	return &Still{Raw: raw, Image: img}, nil
}

// Ping checks the camera is reachable and the credentials work, so a bad host,
// key or camera ID surfaces at startup rather than as a silent gap in the feed
// hours later.
func (c *Client) Ping(ctx context.Context) error {
	url := fmt.Sprintf("https://%s%s/cameras/%s", c.host, apiBase, c.cameraID)

	resp, err := c.do(ctx, url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()                               //nolint:errcheck // read-only body
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16)) //nolint:errcheck // drained for connection reuse

	if resp.StatusCode != http.StatusOK {
		return statusError("ping", resp)
	}
	return nil
}

func (c *Client) do(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("protect: build request: %w", err)
	}
	req.Header.Set("X-API-KEY", c.apiKey)
	req.Header.Set("Accept", "application/json, image/jpeg")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("protect: %w", err)
	}
	return resp, nil
}

func statusError(op string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
	msg := strings.TrimSpace(string(body))
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("protect: %s: %s (check PROTECT_API_KEY): %s", op, resp.Status, msg)
	case http.StatusNotFound:
		return fmt.Errorf("protect: %s: %s (check PROTECT_CAMERA_ID): %s", op, resp.Status, msg)
	default:
		return fmt.Errorf("protect: %s: %s: %s", op, resp.Status, msg)
	}
}
