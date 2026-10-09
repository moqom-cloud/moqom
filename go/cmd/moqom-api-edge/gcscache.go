package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

// gcsCache is an autocert.Cache in a GCS bucket, over the JSON API with the
// pod's Workload Identity token. The bucket holds the ACME account key and the
// certificate's private key, so it is private and only this edge's service
// account may read it.
type gcsCache struct {
	bucket  string
	prefix  string
	storage string
	token   func(context.Context) (string, error)
	client  *http.Client
}

const (
	storageURL  = "https://storage.googleapis.com"
	metadataURL = "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token"

	// Certificates and the account key are a few kilobytes.
	largestCacheEntry = 1 << 20
)

// newGCSCache keeps entries under autocert/<prefix>/, so a certificate from
// one ACME directory (staging) is never served after switching to another.
func newGCSCache(bucket, prefix string) *gcsCache {
	client := &http.Client{Timeout: 30 * time.Second}
	return &gcsCache{
		bucket:  bucket,
		prefix:  "autocert/" + prefix + "/",
		storage: storageURL,
		token:   (&metadataToken{url: metadataURL, client: client}).get,
		client:  client,
	}
}

func (c *gcsCache) object(key string) string {
	return c.storage + "/storage/v1/b/" + url.PathEscape(c.bucket) + "/o/" + url.PathEscape(c.prefix+key)
}

func (c *gcsCache) do(ctx context.Context, method, target string, body []byte) (*http.Response, error) {
	token, err := c.token(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcs cache: token: %w", err)
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		request.Header.Set("Content-Type", "application/octet-stream")
	}
	return c.client.Do(request)
}

func (c *gcsCache) Get(ctx context.Context, key string) ([]byte, error) {
	response, err := c.do(ctx, http.MethodGet, c.object(key)+"?alt=media", nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
		return io.ReadAll(io.LimitReader(response.Body, largestCacheEntry))
	case http.StatusNotFound:
		return nil, autocert.ErrCacheMiss
	default:
		return nil, statusError("get", key, response)
	}
}

func (c *gcsCache) Put(ctx context.Context, key string, data []byte) error {
	target := c.storage + "/upload/storage/v1/b/" + url.PathEscape(c.bucket) +
		"/o?uploadType=media&name=" + url.QueryEscape(c.prefix+key)
	response, err := c.do(ctx, http.MethodPost, target, data)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return statusError("put", key, response)
	}
	return nil
}

func (c *gcsCache) Delete(ctx context.Context, key string) error {
	response, err := c.do(ctx, http.MethodDelete, c.object(key), nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusOK &&
		response.StatusCode != http.StatusNotFound {
		return statusError("delete", key, response)
	}
	return nil
}

func statusError(operation, key string, response *http.Response) error {
	detail, _ := io.ReadAll(io.LimitReader(response.Body, 512))
	return fmt.Errorf("gcs cache: %s %s: %s: %s", operation, key, response.Status, bytes.TrimSpace(detail))
}

// metadataToken is the pod's access token from the GKE metadata server, which
// Workload Identity answers for the bound Google service account. Held until
// a minute before it expires.
type metadataToken struct {
	url    string
	client *http.Client

	mu      sync.Mutex
	value   string
	expires time.Time
}

func (t *metadataToken) get(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.value != "" && time.Until(t.expires) > time.Minute {
		return t.value, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, t.url, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Metadata-Flavor", "Google")
	response, err := t.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metadata server: %s", response.Status)
	}
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&body); err != nil {
		return "", fmt.Errorf("metadata server: %w", err)
	}
	if body.AccessToken == "" {
		return "", fmt.Errorf("metadata server: no access token")
	}
	t.value, t.expires = body.AccessToken, time.Now().Add(time.Duration(body.ExpiresIn)*time.Second)
	return t.value, nil
}
