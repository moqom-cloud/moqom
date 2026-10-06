package recording_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/moqom-cloud/moqom/go/internal/recording"
	"github.com/moqom-cloud/moqom/go/internal/recording/recordingtest"
)

var epoch = time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

func TestMemory(t *testing.T) {
	recordingtest.Run(t, func(*testing.T) recording.Store { return recording.NewMemory() })
}

type rsaSigner struct {
	key *rsa.PrivateKey
}

func (rsaSigner) Email() string { return "reader@project.iam.gserviceaccount.com" }

func (s rsaSigner) SignBlob(_ context.Context, payload []byte) ([]byte, error) {
	digest := sha256.Sum256(payload)
	return rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
}

// The signature verifies over the string GCS rebuilds from the URL, which is
// the only check of a V4 URL that does not need GCS itself.
func TestSignedURLsVerifyAgainstTheirCanonicalRequest(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer := recording.GCSSigner{Signer: rsaSigner{key: key}}
	link, err := signer.SignedURL(context.Background(), "bucket", "acme/room 42/r1/alice/a1-s00000.mqa", 15*time.Minute, epoch)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Host != "storage.googleapis.com" || parsed.EscapedPath() != "/bucket/acme/room%2042/r1/alice/a1-s00000.mqa" {
		t.Fatalf("link is %s", link)
	}
	query := parsed.Query()
	if query.Get("X-Goog-Expires") != "900" || query.Get("X-Goog-Date") != "20260727T120000Z" {
		t.Errorf("expires %s at %s", query.Get("X-Goog-Expires"), query.Get("X-Goog-Date"))
	}
	if want := "reader@project.iam.gserviceaccount.com/20260727/auto/storage/goog4_request"; query.Get("X-Goog-Credential") != want {
		t.Errorf("credential %s", query.Get("X-Goog-Credential"))
	}

	unsigned := strings.Split(parsed.RawQuery, "&X-Goog-Signature=")[0]
	canonical := strings.Join([]string{"GET", parsed.EscapedPath(), unsigned, "host:storage.googleapis.com\n", "host", "UNSIGNED-PAYLOAD"}, "\n")
	digest := sha256.Sum256([]byte(canonical))
	toSign := "GOOG4-RSA-SHA256\n20260727T120000Z\n20260727/auto/storage/goog4_request\n" + hex.EncodeToString(digest[:])
	signature, err := hex.DecodeString(query.Get("X-Goog-Signature"))
	if err != nil {
		t.Fatal(err)
	}
	signed := sha256.Sum256([]byte(toSign))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, signed[:], signature); err != nil {
		t.Errorf("the signature does not verify: %v", err)
	}

	if _, err := signer.SignedURL(context.Background(), "bucket", "x", 8*24*time.Hour, epoch); err == nil {
		t.Error("a link longer than GCS allows was signed")
	}
}

func TestIAMSignerCallsSignBlob(t *testing.T) {
	var asked string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path
		var body struct{ Payload string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		payload, _ := base64.StdEncoding.DecodeString(body.Payload)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"keyId":      "k",
			"signedBlob": base64.StdEncoding.EncodeToString(append([]byte("signed:"), payload...)),
		})
	}))
	defer server.Close()

	signer := recording.IAMSigner{Account: "reader@p.iam.gserviceaccount.com", Client: server.Client(), Endpoint: server.URL}
	signature, err := signer.SignBlob(context.Background(), []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if string(signature) != "signed:hello" {
		t.Errorf("signature %q", signature)
	}
	if asked != "/v1/projects/-/serviceAccounts/reader@p.iam.gserviceaccount.com:signBlob" {
		t.Errorf("asked %s", asked)
	}
}
