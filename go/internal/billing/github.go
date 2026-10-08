package billing

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GitHub's secret scanning partner program: when a string matching mqk_ is
// pushed somewhere public, GitHub posts it here, signed. Anything in such a
// report is public and must be treated as compromised, so a key that verifies
// is revoked at once, without the last-key confirmation a tenant would get:
// a leaked last key stopping service is the lesser harm.

// GitHubSecretScanningPath is the partner endpoint registered with GitHub.
const GitHubSecretScanningPath = "/v1/github/secret-scanning"

// GitHubSecretScanningKeysURL publishes the keys GitHub signs reports with.
const GitHubSecretScanningKeysURL = "https://api.github.com/meta/public_keys/secret_scanning"

// ErrGitHubSignature is a report that is not provably from GitHub.
var ErrGitHubSignature = errors.New("github signature")

// GitHubKeySource finds the public key a report names.
type GitHubKeySource interface {
	Key(ctx context.Context, identifier string) (*ecdsa.PublicKey, error)
}

// GitHubKeys fetches GitHub's secret scanning keys and caches them for as long
// as Cache-Control allows, between five minutes and a day. An unknown
// identifier refetches at most once a minute, so a forged header naming a
// made-up key cannot turn every request into one to GitHub, which also keeps
// within GitHub's unauthenticated rate limit.
type GitHubKeys struct {
	URL    string
	Client *http.Client
	Now    func() time.Time

	mu      sync.Mutex
	keys    map[string]*ecdsa.PublicKey
	expires time.Time
	fetched time.Time
}

func (g *GitHubKeys) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g *GitHubKeys) Key(ctx context.Context, identifier string) (*ecdsa.PublicKey, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if key, ok := g.keys[identifier]; ok && now.Before(g.expires) {
		return key, nil
	}
	if g.keys == nil || !now.Before(g.expires) || now.Sub(g.fetched) >= time.Minute {
		if err := g.refresh(ctx, now); err != nil && g.keys == nil {
			return nil, err
		}
	}
	if key, ok := g.keys[identifier]; ok {
		return key, nil
	}
	return nil, fmt.Errorf("%w: unknown key identifier", ErrGitHubSignature)
}

func (g *GitHubKeys) refresh(ctx context.Context, now time.Time) error {
	g.fetched = now
	client := g.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, g.URL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching %s: %s", g.URL, response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	keys, err := ParseGitHubKeys(body)
	if err != nil {
		return err
	}
	ttl := 5 * time.Minute
	for _, directive := range strings.Split(response.Header.Get("Cache-Control"), ",") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(directive), "max-age="); ok {
			if seconds, err := strconv.Atoi(value); err == nil {
				ttl = min(max(time.Duration(seconds)*time.Second, 5*time.Minute), 24*time.Hour)
			}
		}
	}
	g.keys, g.expires = keys, now.Add(ttl)
	return nil
}

// ParseGitHubKeys reads the ECDSA P-256 keys from GitHub's key list. A key of
// any other type is skipped, not trusted.
func ParseGitHubKeys(body []byte) (map[string]*ecdsa.PublicKey, error) {
	var list struct {
		PublicKeys []struct {
			KeyIdentifier string `json:"key_identifier"`
			Key           string `json:"key"`
		} `json:"public_keys"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	keys := map[string]*ecdsa.PublicKey{}
	for _, entry := range list.PublicKeys {
		block, _ := pem.Decode([]byte(entry.Key))
		if block == nil || entry.KeyIdentifier == "" {
			continue
		}
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			continue
		}
		if key, ok := parsed.(*ecdsa.PublicKey); ok && key.Curve.Params().Name == "P-256" {
			keys[entry.KeyIdentifier] = key
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("github published no usable secret scanning keys")
	}
	return keys, nil
}

// VerifyGitHubSignature checks ECDSA-NIST-P256V1-SHA256 over the raw body, as
// it arrived: re-encoding the JSON first would change the bytes signed.
func VerifyGitHubSignature(ctx context.Context, keys GitHubKeySource, body []byte, identifier, signature string) error {
	if identifier == "" || signature == "" {
		return fmt.Errorf("%w: missing", ErrGitHubSignature)
	}
	der, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return fmt.Errorf("%w: not base64", ErrGitHubSignature)
	}
	key, err := keys.Key(ctx, identifier)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(body)
	if !ecdsa.VerifyASN1(key, digest[:], der) {
		return fmt.Errorf("%w: does not verify", ErrGitHubSignature)
	}
	return nil
}

// SecretMatch is one string GitHub found.
type SecretMatch struct {
	Token  string `json:"token"`
	Type   string `json:"type"`
	URL    string `json:"url"`
	Source string `json:"source"`
}

// SecretLabel is the feedback GitHub accepts for a match. The token goes back
// hashed, so the response never repeats a secret.
type SecretLabel struct {
	TokenHash string `json:"token_hash"`
	TokenType string `json:"token_type"`
	Label     string `json:"label"`
}

const mostSecretMatches = 1000

// HandleSecretScanning revokes every reported key whose secret verifies and
// labels each match. Notices go out after the response, because GitHub waits
// on it and an SMTP server need not be quick.
func (s *Service) HandleSecretScanning(ctx context.Context, body []byte, address string) ([]SecretLabel, error) {
	var matches []SecretMatch
	if err := json.Unmarshal(body, &matches); err != nil {
		return nil, errors.Join(ErrInvalid, err)
	}
	if len(matches) > mostSecretMatches {
		return nil, fmt.Errorf("%w: at most %d matches per report", ErrInvalid, mostSecretMatches)
	}
	labels := make([]SecretLabel, 0, len(matches))
	var leaks []LeakedKey
	for _, match := range matches {
		digest := sha256.Sum256([]byte(match.Token))
		label := SecretLabel{TokenHash: hex.EncodeToString(digest[:]), TokenType: match.Type, Label: "false_positive"}
		leak, found, err := s.revokeLeaked(ctx, match, address)
		if err != nil {
			return nil, err
		}
		if found {
			label.Label = "true_positive"
			if leak.WasWorking {
				leaks = append(leaks, leak)
			}
		}
		labels = append(labels, label)
	}
	if len(leaks) > 0 {
		go s.noticeLeaks(context.WithoutCancel(ctx), leaks)
	}
	return labels, nil
}

// revokeLeaked revokes one reported key, if it is one. A key that was already
// revoked is still a true positive, and is not noticed twice.
func (s *Service) revokeLeaked(ctx context.Context, match SecretMatch, address string) (LeakedKey, bool, error) {
	id, secret, ok := ParseKey(strings.TrimSpace(match.Token))
	if !ok {
		return LeakedKey{}, false, nil
	}
	key, err := s.store.GetKey(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return LeakedKey{}, false, nil
	}
	if err != nil {
		return LeakedKey{}, false, err
	}
	if !s.keys.Verify(key, secret) {
		return LeakedKey{}, false, nil
	}
	now := s.now()
	leak := LeakedKey{Key: key, URL: match.URL, Source: match.Source, RevokedAt: now, WasWorking: key.WorksAt(now)}
	if !key.RevokedAsOf(now) {
		if err := s.store.RevokeKey(ctx, key.Tenant, key.ID, now); err != nil {
			return LeakedKey{}, false, err
		}
	}
	detail := strings.TrimSpace("github " + match.Source + " " + match.URL)
	if len(detail) > 500 {
		detail = detail[:500]
	}
	// Written now rather than buffered: this entry is the one a tenant will
	// look for, and it must survive this replica stopping a second later.
	use := KeyUse{Key: key.ID, Tenant: key.Tenant, Minute: now.UTC().Truncate(time.Minute),
		Operation: "POST " + GitHubSecretScanningPath, Address: address, Outcome: OutcomeLeaked,
		Detail: detail, Count: 1, FirstAt: now, LastAt: now}
	if err := s.store.RecordKeyUse(ctx, []KeyUse{use}); err != nil {
		s.log.ErrorContext(ctx, "leaked key revoked but not audited", "key", key.ID, "tenant", key.Tenant, "error", err)
	}
	// The compliance module can alert on this event.
	s.log.WarnContext(ctx, "api key leaked and revoked", "event", "api_key_leaked",
		"key", key.ID, "tenant", key.Tenant, "kind", key.Kind, "url", match.URL, "source", match.Source,
		"was_working", leak.WasWorking)
	return leak, true, nil
}

const leakNoticeTimeout = 15 * time.Second

func (s *Service) noticeLeaks(ctx context.Context, leaks []LeakedKey) {
	notifier, ok := s.notifier.(KeyLeakNotifier)
	if !ok {
		return
	}
	for _, leak := range leaks {
		tenant, err := s.store.GetTenant(ctx, leak.Key.Tenant)
		if err != nil {
			s.log.ErrorContext(ctx, "leaked key notice not sent", "event", "api_key_leak_notice_failed",
				"key", leak.Key.ID, "error", err)
			continue
		}
		leak.Tenant = tenant
		deliver, cancel := context.WithTimeout(ctx, leakNoticeTimeout)
		if err := notifier.NotifyLeakedKey(deliver, leak); err != nil {
			s.log.ErrorContext(ctx, "leaked key notice not sent", "event", "api_key_leak_notice_failed",
				"key", leak.Key.ID, "tenant", leak.Key.Tenant, "error", err)
		}
		cancel()
	}
}

// secretScanning serves the partner endpoint. Signature first, then parse:
// nothing in an unverified body is read.
func (h *handler) secretScanning() http.HandlerFunc {
	// GitHub sends a handful of reports a day; this bounds what somebody
	// forging them can make the key fetch and the store do.
	limiter := &windowLimiter{per: 120, window: time.Minute, now: h.service.now}
	return func(w http.ResponseWriter, r *http.Request) {
		address := clientAddress(r)
		if !limiter.permit(address) {
			w.Header().Set("Retry-After", "60")
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many requests from this address"})
			return
		}
		payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, largestBody))
		if err != nil {
			writeError(w, errors.Join(ErrInvalid, err))
			return
		}
		if err := VerifyGitHubSignature(r.Context(), h.service.github, payload,
			r.Header.Get("Github-Public-Key-Identifier"), r.Header.Get("Github-Public-Key-Signature")); err != nil {
			h.service.log.WarnContext(r.Context(), "github secret scanning signature rejected",
				"event", "github_secret_scanning_signature_rejected", "remote", address, "error", err)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "signature"})
			return
		}
		labels, err := h.service.HandleSecretScanning(r.Context(), payload, address)
		if err != nil {
			h.service.log.ErrorContext(r.Context(), "github secret scanning report failed", "error", err)
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, labels)
	}
}
