package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/acme/autocert"
)

// fakeGCS is the three JSON API calls the cache makes, for one bucket.
type fakeGCS struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (f *fakeGCS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer token-1" {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.EscapedPath()
	switch {
	case r.Method == http.MethodPost && path == "/upload/storage/v1/b/moqom-api-acme/o":
		if r.URL.Query().Get("uploadType") != "media" {
			http.Error(w, "uploadType", http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.objects[r.URL.Query().Get("name")] = body
		_, _ = w.Write([]byte(`{}`))
	case strings.HasPrefix(path, "/storage/v1/b/moqom-api-acme/o/"):
		name, err := url.PathUnescape(strings.TrimPrefix(path, "/storage/v1/b/moqom-api-acme/o/"))
		if err != nil {
			http.Error(w, "name", http.StatusBadRequest)
			return
		}
		data, ok := f.objects[name]
		switch {
		case !ok:
			http.Error(w, "No such object", http.StatusNotFound)
		case r.Method == http.MethodGet && r.URL.Query().Get("alt") == "media":
			_, _ = w.Write(data)
		case r.Method == http.MethodDelete:
			delete(f.objects, name)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	default:
		http.Error(w, "unexpected "+r.Method+" "+path, http.StatusBadRequest)
	}
}

func TestGCSCache(t *testing.T) {
	storage := &fakeGCS{objects: map[string][]byte{}}
	storageServer := httptest.NewServer(storage)
	defer storageServer.Close()

	var tokenRequests atomic.Int32
	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			http.Error(w, "flavor", http.StatusForbidden)
			return
		}
		tokenRequests.Add(1)
		_, _ = w.Write([]byte(`{"access_token":"token-1","expires_in":3599,"token_type":"Bearer"}`))
	}))
	defer metadata.Close()

	cache := newGCSCache("moqom-api-acme", "acme-v02.api.letsencrypt.org")
	cache.storage = storageServer.URL
	cache.token = (&metadataToken{url: metadata.URL, client: metadata.Client()}).get
	var _ autocert.Cache = cache
	ctx := context.Background()

	if _, err := cache.Get(ctx, "acme_account+key"); !errors.Is(err, autocert.ErrCacheMiss) {
		t.Fatalf("Get of nothing: %v, want ErrCacheMiss", err)
	}
	if err := cache.Put(ctx, "acme_account+key", []byte("account")); err != nil {
		t.Fatal(err)
	}
	if err := cache.Put(ctx, "api.moqom.cloud", []byte("certificate")); err != nil {
		t.Fatal(err)
	}
	if _, ok := storage.objects["autocert/acme-v02.api.letsencrypt.org/acme_account+key"]; !ok {
		t.Fatalf("stored as %v", storage.objects)
	}
	for key, want := range map[string]string{"acme_account+key": "account", "api.moqom.cloud": "certificate"} {
		got, err := cache.Get(ctx, key)
		if err != nil || string(got) != want {
			t.Fatalf("Get(%s) = %q, %v", key, got, err)
		}
	}
	if err := cache.Delete(ctx, "api.moqom.cloud"); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Get(ctx, "api.moqom.cloud"); !errors.Is(err, autocert.ErrCacheMiss) {
		t.Fatalf("Get after Delete: %v", err)
	}
	if err := cache.Delete(ctx, "api.moqom.cloud"); err != nil {
		t.Fatalf("Delete of nothing: %v", err)
	}
	if n := tokenRequests.Load(); n != 1 {
		t.Fatalf("%d token requests, want 1 held for its lifetime", n)
	}

	// A refusal is an error, not a miss: a miss makes autocert issue again.
	cache.token = func(context.Context) (string, error) { return "wrong", nil }
	if _, err := cache.Get(ctx, "acme_account+key"); err == nil || errors.Is(err, autocert.ErrCacheMiss) {
		t.Fatalf("Get with a bad token: %v", err)
	}
}
