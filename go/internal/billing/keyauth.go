package billing

import (
	"context"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/peer"
)

// Use is one presented key on one call, as much as the transport knows of it.
type Use struct {
	// Operation is the gRPC full method, or the HTTP method and route
	// pattern ("GET /v1/keys/{id}/audit"), never a path with IDs or a query
	// in it.
	Operation string
	// Address is the caller as far as it can be trusted: the gRPC peer, or
	// clientAddress for HTTP. Empty is unknown, which a key with an allowlist
	// is refused on.
	Address string
}

type useKey struct{}

// WithUse attaches what an HTTP route knows about a call, for routes outside
// this package that resolve keys through Accounts.
func WithUse(ctx context.Context, use Use) context.Context {
	return context.WithValue(ctx, useKey{}, use)
}

// UseFrom reads the call a key is presented on: what an HTTP route attached,
// or failing that the gRPC method and peer.
func UseFrom(ctx context.Context) Use {
	if use, ok := ctx.Value(useKey{}).(Use); ok {
		return use
	}
	var use Use
	if method, ok := grpc.Method(ctx); ok {
		use.Operation = method
	}
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		host, _, err := net.SplitHostPort(p.Addr.String())
		if err != nil {
			host = p.Addr.String()
		}
		if ip, err := netip.ParseAddr(host); err == nil {
			use.Address = ip.Unmap().String()
		}
	}
	return use
}

// Annotate wraps the control plane's own HTTP routes (/join, the console's
// rooms and live map), so keys used there are scoped, allowlisted and audited
// by the same trusted address the billing routes use.
func Annotate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		use := Use{Operation: r.Method + " " + r.URL.Path, Address: clientAddress(r)}
		next.ServeHTTP(w, r.WithContext(WithUse(r.Context(), use)))
	})
}

const (
	usageRoute = "POST /v1/usage"
	adminRPC   = "/moqom.admin.v1."
)

// scopes is everything the narrower kinds may call. Anything not listed is
// refused, so an RPC or route added later is closed to them until somebody
// decides otherwise.
var scopes = map[KeyKind]map[string]bool{
	KeyMint: {
		adminRPC + "Tokens/MintClientToken":      true,
		adminRPC + "Tokens/ListVerificationKeys": true,
		adminRPC + "Rooms/GetRoom":               true,
		adminRPC + "Rooms/ListRooms":             true,
		adminRPC + "Rooms/ListParticipants":      true,
		adminRPC + "Rooms/GetParticipant":        true,
		"POST /join":                             true,
	},
	KeyReadOnly: {
		adminRPC + "Tokens/ListVerificationKeys":  true,
		adminRPC + "Rooms/GetRoom":                true,
		adminRPC + "Rooms/ListRooms":              true,
		adminRPC + "Rooms/ListParticipants":       true,
		adminRPC + "Rooms/GetParticipant":         true,
		adminRPC + "Rooms/ListWatchdogs":          true,
		adminRPC + "Moderation/ListModerationLog": true,
		adminRPC + "Events/StreamRoomEvents":      true,
		"GET /v1/billing":                         true,
		"GET /v1/billing/ledger":                  true,
	},
}

// permits decides whether kind may make a call. The backend key is the
// tenant's owner and keeps everything it had: every admin RPC and route but
// usage reporting, including calls whose operation is unknown, which is how
// it worked before scopes existed.
func permits(kind KeyKind, operation string) bool {
	switch kind {
	case KeyBackend:
		return operation != usageRoute
	case KeyRelay:
		return operation == usageRoute
	}
	return scopes[kind][operation]
}

// Outcomes recorded in the key audit.
const (
	OutcomeOK          = "ok"
	OutcomeBadSecret   = "bad_secret"
	OutcomeRevoked     = "revoked"
	OutcomeExpired     = "expired"
	OutcomeOutOfScope  = "out_of_scope"
	OutcomeAddress     = "address_refused"
	OutcomeRateLimited = "rate_limited"
	OutcomeLeaked      = "leaked_revoked"
)

// KeyAuth resolves presented keys and decides whether each use is allowed:
// secret, lifetime, scope, address, rate, in that order, recording each use
// that names a real key in the audit.
type KeyAuth struct {
	Store   Store
	Peppers Peppers
	Now     func() time.Time
	Log     *slog.Logger

	limits keyLimiter
	audit  useBuffer
}

func (a *KeyAuth) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *KeyAuth) log() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.Default()
}

// Authorize returns the key presented for use, or why it may not be used.
// Before the secret is proven every failure is ErrInvalidKey; after, the
// refusal says what it was.
func (a *KeyAuth) Authorize(ctx context.Context, presented string, use Use) (Key, error) {
	id, secret, ok := ParseKey(presented)
	if !ok {
		return Key{}, ErrInvalidKey
	}
	key, err := a.Store.GetKey(ctx, id)
	if err != nil {
		return Key{}, ErrInvalidKey
	}
	now := a.now()
	record := func(outcome string) { a.Record(key, use, outcome, "", now) }

	matched, stale := a.Peppers.verify(key, secret)
	switch {
	case !matched:
		record(OutcomeBadSecret)
		return Key{}, ErrInvalidKey
	case key.RevokedAsOf(now):
		record(OutcomeRevoked)
		return Key{}, ErrInvalidKey
	case key.ExpiredAsOf(now):
		record(OutcomeExpired)
		return Key{}, ErrKeyExpired
	case !permits(key.Kind, use.Operation):
		record(OutcomeOutOfScope)
		if key.Kind == KeyRelay {
			// A relay key has never been a credential anywhere but usage, and
			// clients already treat it as one that is not.
			return Key{}, ErrInvalidKey
		}
		return Key{}, ErrOutOfScope
	case !allowedFrom(key, use.Address):
		record(OutcomeAddress)
		return Key{}, ErrAddressRefused
	case !a.limits.allow(key.ID, key.Limit(), now):
		record(OutcomeRateLimited)
		return Key{}, ErrKeyRateLimited
	}
	if stale {
		a.rehash(ctx, key, secret)
	}
	record(OutcomeOK)
	return key, nil
}

// rehash moves a key to the current pepper now that its secret has been seen.
// Conditional on the old hash, so two replicas doing it at once agree, and
// best effort: a key that fails to move still works and tries again next use.
func (a *KeyAuth) rehash(ctx context.Context, key Key, secret string) {
	hash, version := a.Peppers.hash(secret)
	if err := a.Store.RehashKey(ctx, key.ID, key.Hash, hash, version); err != nil {
		a.log().WarnContext(ctx, "api key not rehashed; it still works and will be retried",
			"key", key.ID, "from_version", key.PepperVersion, "to_version", version, "error", err)
		return
	}
	a.log().InfoContext(ctx, "api key rehashed", "event", "api_key_rehashed",
		"key", key.ID, "from_version", key.PepperVersion, "to_version", version)
}

// Verify reports whether secret is key's, without any of the use checks. For
// a leaked key reported by a scanner, which is to be revoked however it would
// have been used.
func (a *KeyAuth) Verify(key Key, secret string) bool {
	ok, _ := a.Peppers.verify(key, secret)
	return ok
}

func allowedFrom(key Key, address string) bool {
	if len(key.AllowedNetworks) == 0 {
		return true
	}
	ip, err := netip.ParseAddr(address)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	for _, network := range key.AllowedNetworks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// keyLimiter is a token bucket per key, in this replica's memory. A limit is
// therefore per replica: N replicas admit up to N times the stated rate. That
// is the trade for not putting a shared counter in the path of every call;
// what it bounds is one runaway client, which lands on one replica at a time.
type keyLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	at     time.Time
}

// burstSeconds is how much of a minute's allowance may arrive at once.
const burstSeconds = 10

func (l *keyLimiter) allow(id string, perMinute int, now time.Time) bool {
	if perMinute <= 0 {
		return true
	}
	rate := float64(perMinute) / 60
	capacity := math.Max(1, rate*burstSeconds)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buckets == nil {
		l.buckets = map[string]*bucket{}
	}
	if len(l.buckets) >= sweepAbove {
		// A bucket idle long enough to have refilled is the same as no bucket.
		for other, b := range l.buckets {
			if now.Sub(b.at) > time.Hour {
				delete(l.buckets, other)
			}
		}
	}
	b := l.buckets[id]
	if b == nil {
		b = &bucket{tokens: capacity, at: now}
		l.buckets[id] = b
	}
	if elapsed := now.Sub(b.at).Seconds(); elapsed > 0 {
		b.tokens = math.Min(capacity, b.tokens+elapsed*rate)
		b.at = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// KeyUse is how many times one key was used for one operation, from one
// address, with one outcome, within one minute. Counted rather than one row a
// call, so a backend minting a thousand tokens a second writes a handful of
// rows a minute, and the table's size follows how varied the use is rather
// than how heavy.
type KeyUse struct {
	Key       string
	Tenant    string
	Minute    time.Time
	Operation string
	Address   string
	Outcome   string
	// Detail is set on the rare entries a person should read: where a leaked
	// key was found.
	Detail  string
	Count   int64
	FirstAt time.Time
	LastAt  time.Time
}

type useGroup struct {
	key, operation, address, outcome, detail string
	minute                                   time.Time
}

// useBuffer aggregates uses in memory and writes them in batches, off the
// call's path. A crash loses at most a flush interval of counts; the call
// itself never waits on, or fails because of, the audit write.
type useBuffer struct {
	mu       sync.Mutex
	pending  map[useGroup]*KeyUse
	flushing bool
	dropped  int64
}

// Audit buffer bounds. flushAbove starts an early write; most is where new
// groups are dropped and counted rather than held, so a flood of distinct
// addresses cannot exhaust memory.
const (
	KeyAuditFlushInterval = 10 * time.Second
	flushAbove            = 1000
	mostPending           = 20_000
)

// Record counts one use. Exported for uses that are not authorisations, such
// as a scanner's report.
func (a *KeyAuth) Record(key Key, use Use, outcome, detail string, at time.Time) {
	b := &a.audit
	group := useGroup{key: key.ID, operation: use.Operation, address: use.Address,
		outcome: outcome, detail: detail, minute: at.UTC().Truncate(time.Minute)}
	b.mu.Lock()
	if b.pending == nil {
		b.pending = map[useGroup]*KeyUse{}
	}
	entry := b.pending[group]
	switch {
	case entry != nil:
		entry.Count++
		entry.LastAt = at
	case len(b.pending) >= mostPending:
		b.dropped++
	default:
		b.pending[group] = &KeyUse{Key: key.ID, Tenant: key.Tenant, Minute: group.minute,
			Operation: use.Operation, Address: use.Address, Outcome: outcome, Detail: detail,
			Count: 1, FirstAt: at, LastAt: at}
	}
	early := len(b.pending) >= flushAbove && !b.flushing
	if early {
		b.flushing = true
	}
	b.mu.Unlock()
	if early {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = a.Flush(ctx)
		}()
	}
}

// Flush writes everything buffered. What fails to write is put back, merged
// with what arrived meanwhile, for the next flush.
func (a *KeyAuth) Flush(ctx context.Context) error {
	b := &a.audit
	b.mu.Lock()
	batch, dropped := b.pending, b.dropped
	b.pending, b.dropped = nil, 0
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.flushing = false
		b.mu.Unlock()
	}()
	if dropped > 0 {
		a.log().WarnContext(ctx, "api key audit dropped uses; the buffer was full",
			"event", "api_key_audit_dropped", "dropped", dropped)
	}
	if len(batch) == 0 {
		return nil
	}
	uses := make([]KeyUse, 0, len(batch))
	for _, use := range batch {
		uses = append(uses, *use)
	}
	err := a.Store.RecordKeyUse(ctx, uses)
	if err == nil {
		return nil
	}
	a.log().WarnContext(ctx, "api key audit not written; kept for the next flush", "uses", len(uses), "error", err)
	b.mu.Lock()
	if b.pending == nil {
		b.pending = map[useGroup]*KeyUse{}
	}
	for group, use := range batch {
		if current := b.pending[group]; current != nil {
			current.Count += use.Count
			current.FirstAt = use.FirstAt
		} else if len(b.pending) < mostPending {
			b.pending[group] = use
		}
	}
	b.mu.Unlock()
	return err
}

// Run flushes the audit every interval until ctx ends, then once more.
func (a *KeyAuth) Run(ctx context.Context) {
	ticker := time.NewTicker(KeyAuditFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			_ = a.Flush(ctx)
		case <-ctx.Done():
			final, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_ = a.Flush(final)
			cancel()
			return
		}
	}
}
