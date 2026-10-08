package recording

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// MaxURLLifetime is the longest a V4 signed URL may be valid for, which GCS
// sets and refuses beyond.
const MaxURLLifetime = 7 * 24 * time.Hour

// BlobSigner signs bytes as a Google service account.
type BlobSigner interface {
	// Email is the service account the signature is made as.
	Email() string
	SignBlob(ctx context.Context, payload []byte) ([]byte, error)
}

// GCSSigner makes V4 signed download URLs.
//
// The signature comes from the IAM Credentials API rather than a key file:
// under Workload Identity the control plane has no private key to sign with,
// and the account it runs as can be granted signBlob on the one account that
// may read recordings. A leaked URL opens one object for a bounded time; a
// leaked key file would open the bucket until someone noticed.
type GCSSigner struct {
	Signer BlobSigner
	// Host is storage.googleapis.com unless a test says otherwise.
	Host string
}

var _ URLSigner = GCSSigner{}

func (g GCSSigner) SignedURL(ctx context.Context, bucket, object string, lifetime time.Duration, now time.Time) (string, error) {
	if lifetime <= 0 || lifetime > MaxURLLifetime {
		return "", fmt.Errorf("a signed URL lives between a second and %s, not %s", MaxURLLifetime, lifetime)
	}
	host := g.Host
	if host == "" {
		host = "storage.googleapis.com"
	}
	stamp := now.UTC().Format("20060102T150405Z")
	scope := now.UTC().Format("20060102") + "/auto/storage/goog4_request"

	path := "/" + bucket + "/" + escapeObject(object)
	query := map[string]string{
		"X-Goog-Algorithm":     "GOOG4-RSA-SHA256",
		"X-Goog-Credential":    g.Signer.Email() + "/" + scope,
		"X-Goog-Date":          stamp,
		"X-Goog-Expires":       fmt.Sprintf("%d", int64(lifetime/time.Second)),
		"X-Goog-SignedHeaders": "host",
	}
	canonicalQuery := canonical(query)
	request := strings.Join([]string{
		http.MethodGet,
		path,
		canonicalQuery,
		"host:" + host + "\n",
		"host",
		"UNSIGNED-PAYLOAD",
	}, "\n")
	digest := sha256.Sum256([]byte(request))
	toSign := strings.Join([]string{"GOOG4-RSA-SHA256", stamp, scope, hex.EncodeToString(digest[:])}, "\n")

	signature, err := g.Signer.SignBlob(ctx, []byte(toSign))
	if err != nil {
		return "", fmt.Errorf("signing a download URL: %w", err)
	}
	return "https://" + host + path + "?" + canonicalQuery + "&X-Goog-Signature=" + hex.EncodeToString(signature), nil
}

// escapeObject percent-encodes an object name for a path, keeping the slashes
// that make it look like one.
func escapeObject(object string) string {
	parts := strings.Split(object, "/")
	for i, part := range parts {
		parts[i] = strings.ReplaceAll(url.PathEscape(part), "+", "%2B")
	}
	return strings.Join(parts, "/")
}

func canonical(query map[string]string) string {
	keys := make([]string, 0, len(query))
	for key := range query {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, url.QueryEscape(key)+"="+strings.ReplaceAll(url.QueryEscape(query[key]), "+", "%20"))
	}
	return strings.Join(pairs, "&")
}

// IAMSigner signs through the IAM Credentials API as Account, authenticating
// with whatever Client carries -- in production, the pod's Workload Identity.
type IAMSigner struct {
	Account string
	Client  *http.Client
	// Endpoint is https://iamcredentials.googleapis.com unless a test says
	// otherwise.
	Endpoint string
}

var _ BlobSigner = IAMSigner{}

func (s IAMSigner) Email() string { return s.Account }

func (s IAMSigner) SignBlob(ctx context.Context, payload []byte) ([]byte, error) {
	endpoint := s.Endpoint
	if endpoint == "" {
		endpoint = "https://iamcredentials.googleapis.com"
	}
	body, err := json.Marshal(map[string]string{"payload": base64.StdEncoding.EncodeToString(payload)})
	if err != nil {
		return nil, err
	}
	target := endpoint + "/v1/projects/-/serviceAccounts/" + url.PathEscape(s.Account) + ":signBlob"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := s.Client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(response.Body, 1<<16))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("signBlob answered %d: %s", response.StatusCode, strings.TrimSpace(string(answer)))
	}
	var signed struct {
		SignedBlob string `json:"signedBlob"`
	}
	if err := json.Unmarshal(answer, &signed); err != nil {
		return nil, fmt.Errorf("signBlob: %w", err)
	}
	return base64.StdEncoding.DecodeString(signed.SignedBlob)
}
