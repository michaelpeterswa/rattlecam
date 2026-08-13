package protect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// jpegBytes is a small valid JPEG to stand in for a snapshot.
func jpegBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 32, 24))
	for y := range 24 {
		for x := range 32 {
			img.Set(x, y, color.RGBA{R: uint8(x * 8), G: uint8(y * 10), B: 90, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// testServer returns a TLS server plus the hex SHA-256 of its leaf certificate,
// which is what PROTECT_CERT_SHA256 holds in production.
func testServer(t *testing.T, h http.Handler) (host, fingerprint string) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)

	leaf := srv.Certificate()
	sum := sha256.Sum256(leaf.Raw)
	return strings.TrimPrefix(srv.URL, "https://"), hex.EncodeToString(sum[:])
}

func snapshotHandler(t *testing.T, body []byte) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-KEY") != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/snapshot"):
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(body)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"cam-1","name":"Tower"}`))
		}
	})
}

// The correct fingerprint must let the connection through.
func TestPinnedCertificateAccepted(t *testing.T) {
	body := jpegBytes(t)
	host, fp := testServer(t, snapshotHandler(t, body))

	c, err := New(host, "secret", "cam-1", fp)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	still, err := c.Snapshot(context.Background(), true)
	if err != nil {
		t.Fatalf("Snapshot with a matching pin: %v", err)
	}
	if got := still.Image.Bounds(); got.Dx() != 32 || got.Dy() != 24 {
		t.Errorf("bounds = %v, want 32x24", got)
	}
	// The clean master and the archive are published from Raw, so it has to be
	// exactly what the camera sent, not a re-encode of it.
	if !bytes.Equal(still.Raw, body) {
		t.Errorf("Raw is %d bytes, want the %d the camera sent, byte for byte",
			len(still.Raw), len(body))
	}
}

// And the wrong one must fail closed. This is the direction that matters: a
// device swap on the LAN could otherwise silently substitute the image feeding a
// newsroom.
func TestPinnedCertificateMismatchFailsClosed(t *testing.T) {
	host, _ := testServer(t, snapshotHandler(t, jpegBytes(t)))

	wrong := strings.Repeat("ab", sha256.Size)
	c, err := New(host, "secret", "cam-1", wrong)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := c.Snapshot(context.Background(), true); err == nil {
		t.Fatal("snapshot succeeded against a certificate that does not match the pin")
	} else if !strings.Contains(err.Error(), "pin mismatch") {
		t.Errorf("error %q does not identify the pin mismatch", err)
	}

	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("ping succeeded against a certificate that does not match the pin")
	}
}

// An unset fingerprint keeps working against the self-signed console, which is
// the documented fallback.
func TestUnpinnedStillConnects(t *testing.T) {
	host, _ := testServer(t, snapshotHandler(t, jpegBytes(t)))

	c, err := New(host, "secret", "cam-1", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Snapshot(context.Background(), true); err != nil {
		t.Fatalf("Snapshot without a pin: %v", err)
	}
}

func TestPing(t *testing.T) {
	host, fp := testServer(t, snapshotHandler(t, jpegBytes(t)))

	c, err := New(host, "secret", "cam-1", fp)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

// A bad key has to be legible at startup rather than showing up as a gap in the
// feed hours later.
func TestPingBadKeyNamesTheVariable(t *testing.T) {
	host, fp := testServer(t, snapshotHandler(t, jpegBytes(t)))

	c, err := New(host, "wrong", "cam-1", fp)
	if err != nil {
		t.Fatal(err)
	}
	err = c.Ping(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "PROTECT_API_KEY") {
		t.Errorf("error %q does not point at the credential", err)
	}
}

func TestSnapshotRejectsNonImage(t *testing.T) {
	host, fp := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"error":"camera offline"}`))
	}))

	c, err := New(host, "secret", "cam-1", fp)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Snapshot(context.Background(), true)
	if err == nil {
		t.Fatal("want an error for a JSON body on the snapshot endpoint")
	}
	if !strings.Contains(err.Error(), "not an image") {
		t.Errorf("error %q does not explain the content type", err)
	}
}

func TestSnapshotURLShape(t *testing.T) {
	var gotPath, gotQuery string
	host, fp := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(jpegBytes(t))
	}))

	c, err := New(host, "secret", "cam-1", fp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Snapshot(context.Background(), true); err != nil {
		t.Fatal(err)
	}

	if want := "/proxy/protect/integration/v1/cameras/cam-1/snapshot"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if want := "highQuality=true"; gotQuery != want {
		t.Errorf("query = %q, want %q", gotQuery, want)
	}
}

func TestParseFingerprint(t *testing.T) {
	raw := strings.Repeat("ab", sha256.Size)

	// openssl prints colon-separated uppercase, prefixed with a label.
	colons := strings.ToUpper(strings.TrimSuffix(strings.Repeat("AB:", sha256.Size), ":"))
	for _, in := range []string{raw, colons, "SHA256 Fingerprint=" + colons} {
		got, err := parseFingerprint(in)
		if err != nil {
			t.Errorf("parseFingerprint(%.24q): %v", in, err)
			continue
		}
		if hex.EncodeToString(got) != raw {
			t.Errorf("parseFingerprint(%.24q) = %x, want %s", in, got, raw)
		}
	}
}

func TestParseFingerprintRejectsWrongLength(t *testing.T) {
	if _, err := parseFingerprint("abcd"); err == nil {
		t.Fatal("want an error for a short fingerprint")
	}
	if _, err := parseFingerprint("nothex!!"); err == nil {
		t.Fatal("want an error for non-hex input")
	}
}

func TestNewValidatesRequiredFields(t *testing.T) {
	for _, tc := range []struct{ host, key, cam, want string }{
		{"", "k", "c", "host"},
		{"h", "", "c", "api key"},
		{"h", "k", "", "camera id"},
	} {
		_, err := New(tc.host, tc.key, tc.cam, "")
		if err == nil {
			t.Errorf("New(%q,%q,%q) succeeded, want an error", tc.host, tc.key, tc.cam)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("error %q does not mention %q", err, tc.want)
		}
	}
}

// The host is configured without a scheme, but pasting one in should not break.
func TestNewStripsScheme(t *testing.T) {
	for _, in := range []string{"192.168.1.1", "https://192.168.1.1", "https://192.168.1.1/"} {
		c, err := New(in, "k", "c", "")
		if err != nil {
			t.Fatalf("New(%q): %v", in, err)
		}
		if c.host != "192.168.1.1" {
			t.Errorf("New(%q).host = %q, want %q", in, c.host, "192.168.1.1")
		}
	}
}

// Raw is only safe to publish unmodified if it is known to be a real image, so
// a body that decodes must be rejected rather than passed on.
func TestSnapshotRejectsUndecodableBody(t *testing.T) {
	host, fp := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("this is not a jpeg"))
	}))

	c, err := New(host, "secret", "cam-1", fp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Snapshot(context.Background(), true); err == nil {
		t.Fatal("a body labelled image/jpeg but undecodable was accepted")
	}
}

// Raw and Image must describe the same frame: publishing one while compositing
// the other would put a different picture in the clean master.
func TestRawAndImageAgree(t *testing.T) {
	body := jpegBytes(t)
	host, fp := testServer(t, snapshotHandler(t, body))

	c, err := New(host, "secret", "cam-1", fp)
	if err != nil {
		t.Fatal(err)
	}
	still, err := c.Snapshot(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}

	decoded, err := jpeg.Decode(bytes.NewReader(still.Raw))
	if err != nil {
		t.Fatalf("Raw does not decode: %v", err)
	}
	if decoded.Bounds() != still.Image.Bounds() {
		t.Errorf("Raw decodes to %v but Image is %v", decoded.Bounds(), still.Image.Bounds())
	}
}
