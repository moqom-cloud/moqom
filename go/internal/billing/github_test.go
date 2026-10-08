package billing

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"mime/quotedprintable"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGitHub publishes a secret scanning key the way api.github.com does.
type fakeGitHub struct {
	key     *ecdsa.PrivateKey
	id      string
	fetches atomic.Int32
}

func newFakeGitHub(t *testing.T) (*fakeGitHub, *GitHubKeys) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	g := &fakeGitHub{key: key, id: "bcb53661c06b4728e59d897fb6165d5c9cda0fd9cdf9d09ead458168deb7518c"}
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	published, _ := json.Marshal(map[string]any{"public_keys": []map[string]any{
		{"key_identifier": g.id, "key": string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), "is_current": true},
	}})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.fetches.Add(1)
		w.Header().Set("Cache-Control", "max-age=3600")
		_, _ = w.Write(published)
	}))
	t.Cleanup(server.Close)
	return g, &GitHubKeys{URL: server.URL, Client: server.Client()}
}

func (g *fakeGitHub) sign(t *testing.T, body []byte) string {
	t.Helper()
	digest := sha256.Sum256(body)
	signature, err := ecdsa.SignASN1(rand.Reader, g.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(signature)
}

func (g *fakeGitHub) post(handler http.Handler, body []byte, identifier, signature string) (*httptest.ResponseRecorder, []SecretLabel) {
	request := httptest.NewRequest("POST", GitHubSecretScanningPath, bytes.NewReader(body))
	request.RemoteAddr = "140.82.115.10:443"
	request.Header.Set("Github-Public-Key-Identifier", identifier)
	request.Header.Set("Github-Public-Key-Signature", signature)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	var labels []SecretLabel
	_ = json.Unmarshal(recorder.Body.Bytes(), &labels)
	return recorder, labels
}

type leakRecorder struct {
	LogNotifier
	leaks chan LeakedKey
}

func (l *leakRecorder) NotifyLeakedKey(_ context.Context, leak LeakedKey) error {
	l.leaks <- leak
	return nil
}

func TestGitHubSecretScanningRevokesVerifiedKeys(t *testing.T) {
	ctx := context.Background()
	github, keys := newFakeGitHub(t)
	notices := &leakRecorder{leaks: make(chan LeakedKey, 4)}
	f := newFixtureWith(t, func(c *Config) {
		c.GitHub = keys
		c.Notifier = notices
		c.Peppers = mustPeppers(t, newPepper(t))
	})
	handler := f.service.Handler(nil)
	signup, _ := f.service.Signup(ctx, SignupRequest{Email: "dev@example.com", Name: "Example"})
	leaked := signup.APIKey
	_, revoked, _ := f.service.IssueKey(ctx, signup.Tenant.ID, "old", KeyBackend)
	revokedID, _, _ := ParseKey(revoked)
	_ = f.store.RevokeKey(ctx, signup.Tenant.ID, revokedID, f.now.Add(-time.Hour))

	report := []SecretMatch{
		{Token: leaked, Type: "moqom_api_key", URL: "https://github.com/acme/app/blob/abc/.env", Source: "content"},
		{Token: alterSecret(leaked), Type: "moqom_api_key", Source: "commit"},
		{Token: "mqk_0000000000000000_nope", Type: "moqom_api_key", Source: "commit"},
		{Token: "mqk_garbage", Type: "moqom_api_key", Source: "commit"},
		{Token: revoked, Type: "moqom_api_key", Source: "issue_comment"},
	}
	body, _ := json.Marshal(report)

	if recorder, _ := github.post(handler, body, github.id, ""); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned: %d", recorder.Code)
	}
	if recorder, _ := github.post(handler, body, "unknown", github.sign(t, body)); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unknown key identifier: %d", recorder.Code)
	}
	tampered := bytes.Replace(body, []byte("content"), []byte("commit!"), 1)
	if recorder, _ := github.post(handler, tampered, github.id, github.sign(t, body)); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("tampered body: %d", recorder.Code)
	}
	if _, err := f.service.Keys().Authorize(ctx, leaked, Use{Operation: "GET /v1/keys"}); err != nil {
		t.Fatalf("a rejected report revoked the key: %v", err)
	}

	recorder, labels := github.post(handler, body, github.id, github.sign(t, body))
	if recorder.Code != http.StatusOK || len(labels) != len(report) {
		t.Fatalf("%d %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "mqk_") {
		t.Fatal("the response repeats a token")
	}
	want := []string{"true_positive", "false_positive", "false_positive", "false_positive", "true_positive"}
	for i, label := range labels {
		digest := sha256.Sum256([]byte(report[i].Token))
		if label.Label != want[i] || label.TokenHash != hex.EncodeToString(digest[:]) || label.TokenType != "moqom_api_key" {
			t.Errorf("label %d: %+v, want %s", i, label, want[i])
		}
	}

	if _, err := f.service.Keys().Authorize(ctx, leaked, Use{Operation: "GET /v1/keys"}); err == nil {
		t.Fatal("the leaked key still works")
	}
	select {
	case leak := <-notices.leaks:
		if leak.Key.ID != signup.Key.ID || leak.Tenant.Email != "dev@example.com" || leak.URL != report[0].URL || !leak.WasWorking {
			t.Fatalf("notice %+v", leak)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no notice sent")
	}
	select {
	case leak := <-notices.leaks:
		t.Fatalf("a key already revoked was noticed: %+v", leak)
	case <-time.After(100 * time.Millisecond):
	}

	uses, _ := f.store.ListKeyUse(ctx, signup.Tenant.ID, signup.Key.ID, 0)
	if len(uses) == 0 || uses[0].Outcome != OutcomeLeaked || !strings.Contains(uses[0].Detail, report[0].URL) {
		t.Fatalf("audit %+v", uses)
	}
	if github.fetches.Load() > 2 {
		t.Fatalf("fetched GitHub's keys %d times", github.fetches.Load())
	}
}

func TestGitHubKeysRefetchUnknownIdentifiersAtMostOnceAMinute(t *testing.T) {
	github, keys := newFakeGitHub(t)
	now := time.Unix(1_800_000_000, 0)
	keys.Now = func() time.Time { return now }
	ctx := context.Background()
	if _, err := keys.Key(ctx, github.id); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		_, _ = keys.Key(ctx, "made-up")
	}
	if n := github.fetches.Load(); n != 1 {
		t.Fatalf("%d fetches for made-up identifiers within a minute", n)
	}
	now = now.Add(time.Minute)
	_, _ = keys.Key(ctx, "made-up")
	if n := github.fetches.Load(); n != 2 {
		t.Fatalf("%d fetches after a minute", n)
	}
}

func TestLeakedKeyMessageNamesTheKeyAndNotTheSecret(t *testing.T) {
	leak := LeakedKey{
		Tenant:    Tenant{ID: "t_1", Name: "Acme", Email: "dev@example.com"},
		Key:       Key{ID: "0123456789abcdef", Name: "prod\r\nBcc: x@evil.test", Kind: KeyBackend, Hash: "stored-hash"},
		URL:       "https://github.com/acme/app/blob/abc/.env",
		Source:    "pull_request_comment",
		RevokedAt: time.Unix(1_800_000_000, 0),
	}
	message, err := leakedKeyMessage(leak, "billing@moqom.cloud", "dev@example.com", "https://moqom.cloud/account", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	headers, encoded, _ := strings.Cut(string(message), "\r\n\r\n")
	if strings.Contains(headers, "\r\nBcc:") {
		t.Fatalf("a key name added a header:\n%s", headers)
	}
	decoded, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	text := string(decoded)
	for _, want := range []string{"0123456789abcdef", "github.com/acme/app", "pull request comment", "/v1/keys/0123456789abcdef/audit"} {
		if !strings.Contains(text, want) {
			t.Errorf("message lacks %q", want)
		}
	}
	if strings.Contains(text, "stored-hash") {
		t.Error("the message contains the stored hash")
	}
}
