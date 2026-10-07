package billing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/moqom-cloud/moqom/go/internal/server"
)

// Handler serves billing over HTTPS.
//
//	GET    /v1/rates                public rate card
//	GET    /v1/public/stats         signup and active-tenant counts, cached 60s
//	POST   /v1/signup               new tenant, first API key, first checkout
//	POST   /v1/stripe/webhook       Stripe, signature-verified
//	GET    /v1/billing              balance, status, recent ledger   (backend key)
//	GET    /v1/billing/ledger       up to 5000 ledger entries         (backend key)
//	POST   /v1/billing/checkout     open a top-up                     (backend key)
//	POST   /v1/billing/portal       Stripe customer portal            (backend key)
//	PUT    /v1/billing/auto-top-up  configure automatic top-ups       (backend key)
//	PUT    /v1/billing/spend-limit  set or clear the monthly limit    (backend key)
//	PUT    /v1/billing/spend-alerts set monthly spend alert amounts   (backend key)
//	GET    /v1/keys                 list keys                         (backend key)
//	POST   /v1/keys                 issue a key, shown once           (backend key)
//	DELETE /v1/keys/{id}            revoke a key; the last working one (backend key)
//	                                needs ?confirm=stop-service
//	POST   /v1/keys/{id}/rotate     issue a replacement; old key works (backend key)
//	                                for overlap_seconds (default 24h)
//	GET    /v1/keys/{id}/audit      one key's use, per minute         (backend key)
//	GET    /v1/audit                every key's use, per minute       (backend key)
//	POST   /v1/github/secret-scanning GitHub partner reports, signature-verified;
//	                                verified mqk_ keys are revoked at once
//	GET    /v1/account/export       DSAR export of the account        (backend key)
//	POST   /v1/usage                report usage                      (relay key or MOQOM relay)
//	                                 "hosting": "cloudflare" with a relay key reports
//	                                 usage on Cloudflare's relays
//	GET    /v1/usage/summary        usage by role and tier for        (backend key)
//	                                ?from&to, beside Agora's list
//	POST   /v1/usage/estimate       what-if scenario, priced          (backend key)
//	POST   /v1/migrate/agora/preview Agora projects and usage, read   (backend key)
//	                                with credentials used once
//
// "backend key" routes take only the owner backend key, except GET /v1/billing
// and GET /v1/billing/ledger, which a readonly key may also read; see permits.
//
// operators resolves MOQOM's own relay credentials, which report usage for
// every tenant; nil accepts only tenant keys.
func (s *Service) Handler(operators server.Accounts) http.Handler {
	h := &handler{service: s, operators: operators,
		signups: &windowLimiter{per: 5, window: time.Hour, now: s.now},
		stats:   &statsCache{limiter: &windowLimiter{per: 120, window: time.Minute, now: s.now}},
		// Each preview makes up to 21 calls to Agora on the tenant's account.
		agoraPreviews: &windowLimiter{per: 6, window: time.Hour, now: s.now},
		agoraInFlight: make(chan struct{}, 4)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/rates", h.rates)
	mux.HandleFunc("GET "+PublicStatsPath, h.publicStats)
	mux.HandleFunc("POST /v1/signup", h.signup)
	mux.HandleFunc("POST /v1/stripe/webhook", h.webhook)
	mux.HandleFunc("GET /v1/billing", h.backend(h.summary))
	mux.HandleFunc("GET /v1/billing/ledger", h.backend(h.ledger))
	mux.HandleFunc("POST /v1/billing/checkout", h.backend(h.checkout))
	mux.HandleFunc("POST /v1/billing/portal", h.backend(h.portal))
	mux.HandleFunc("PUT /v1/billing/auto-top-up", h.backend(h.autoTopUp))
	mux.HandleFunc("PUT /v1/billing/spend-limit", h.backend(h.spendLimit))
	mux.HandleFunc("PUT /v1/billing/spend-alerts", h.backend(h.spendAlerts))
	mux.HandleFunc("GET /v1/keys", h.backend(h.listKeys))
	mux.HandleFunc("POST /v1/keys", h.backend(h.issueKey))
	mux.HandleFunc("DELETE /v1/keys/{id}", h.backend(h.revokeKey))
	mux.HandleFunc("POST /v1/keys/{id}/rotate", h.backend(h.rotateKey))
	mux.HandleFunc("GET /v1/keys/{id}/audit", h.backend(h.keyAudit))
	mux.HandleFunc("GET /v1/audit", h.backend(h.keyAudit))
	mux.HandleFunc("POST "+GitHubSecretScanningPath, h.secretScanning())
	mux.HandleFunc("GET /v1/account/export", h.backend(h.export))
	mux.HandleFunc("POST /v1/usage", h.usage)
	mux.HandleFunc("GET /v1/usage/summary", h.backend(h.usageSummary))
	mux.HandleFunc("POST /v1/usage/estimate", h.backend(h.usageEstimate))
	mux.HandleFunc("POST /v1/migrate/agora/preview", h.backend(h.agoraPreview))
	return mux
}

type handler struct {
	service       *Service
	operators     server.Accounts
	signups       *windowLimiter
	stats         *statsCache
	agoraPreviews *windowLimiter
	agoraInFlight chan struct{}
}

// PublicStatsPath is the one billing route that answers any browser origin
// on the allow-list without a key.
const PublicStatsPath = "/v1/public/stats"

// statsTTL bounds how often a page view can reach the store: one count per
// replica per minute, however many visitors there are.
const statsTTL = time.Minute

type statsCache struct {
	limiter *windowLimiter

	mu   sync.Mutex
	at   time.Time
	body []byte
}

const largestBody = 1 << 20

func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, largestBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		writeError(w, errors.Join(ErrInvalid, err))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	var stripeErr *StripeError
	switch {
	case errors.Is(err, ErrInvalid):
		code = http.StatusBadRequest
	case errors.Is(err, ErrInvalidKey):
		code = http.StatusUnauthorized
	case errors.Is(err, ErrOutOfScope), errors.Is(err, ErrAddressRefused):
		code = http.StatusForbidden
	case errors.Is(err, ErrKeyRateLimited):
		code = http.StatusTooManyRequests
		w.Header().Set("Retry-After", "1")
	case errors.Is(err, ErrPaymentRequired), errors.Is(err, ErrInsufficientCredit), errors.Is(err, ErrSpendLimitReached):
		code = http.StatusPaymentRequired
	case errors.Is(err, ErrSuspended):
		code = http.StatusForbidden
	case errors.Is(err, ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, ErrAlreadyExists), errors.Is(err, ErrTransition):
		code = http.StatusConflict
	case errors.Is(err, ErrPaymentsUnavailable):
		code = http.StatusServiceUnavailable
	case errors.As(err, &stripeErr):
		code = http.StatusBadGateway
	}
	message := err.Error()
	if code == http.StatusInternalServerError {
		message = "internal error"
	}
	writeJSON(w, code, map[string]string{"error": message})
}

func bearer(r *http.Request) string {
	value := r.Header.Get("Authorization")
	if credential, ok := strings.CutPrefix(value, "Bearer "); ok {
		return strings.TrimSpace(credential)
	}
	if credential, ok := strings.CutPrefix(value, "bearer "); ok {
		return strings.TrimSpace(credential)
	}
	return ""
}

func (h *handler) backend(next func(http.ResponseWriter, *http.Request, Key)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, err := h.service.keys.Authorize(r.Context(), bearer(r), Use{Operation: r.Pattern, Address: clientAddress(r)})
		if err != nil {
			writeError(w, err)
			return
		}
		next(w, r, key)
	}
}

type money struct {
	Micros  int64  `json:"micros"`
	Display string `json:"display"`
}

func moneyOf(m Micros) money { return money{Micros: int64(m), Display: m.String()} }

func (h *handler) rates(w http.ResponseWriter, _ *http.Request) {
	pricing := h.service.Pricing()
	ceiling := map[Role]map[Tier]money{}
	for role, tiers := range pricing.Ceiling {
		ceiling[role] = map[Tier]money{}
		for tier, rate := range tiers {
			ceiling[role][tier] = moneyOf(rate)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"currency":                             "usd",
		"model":                                "prepaid, cost-plus, capped",
		"egress_cost_per_gib":                  moneyOf(pricing.Costs.EgressPerGiB),
		"markup_basis_points":                  pricing.MarkupBasisPoints,
		"platform_fee_per_gib":                 moneyOf(pricing.PlatformFeePerGiB),
		"self_hosted_fee_per_gib":              moneyOf(pricing.SelfHostedFeePerGiB),
		"ceiling_per_1000_participant_minutes": ceiling,
		"direct_free_minutes_per_month":        pricing.DirectFreeMinutesPerMonth,
		"direct_fee_per_1000_minutes":          moneyOf(pricing.DirectFeePer1000Minutes),
	})
}

// publicStats answers counts only. Nothing that identifies a tenant is read,
// so nothing that identifies one can leak.
func (h *handler) publicStats(w http.ResponseWriter, r *http.Request) {
	if !h.stats.limiter.permit(clientAddress(r)) {
		w.Header().Set("Retry-After", "60")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many requests from this address"})
		return
	}
	body, err := h.stats.read(r.Context(), h.service.store, h.service.now)
	if err != nil {
		h.service.log.ErrorContext(r.Context(), "public stats failed", "error", err)
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// read returns the cached body while it is fresh, and the stale one if the
// store fails, so a database blip does not blank a marketing page; the stale
// body is then kept for another full TTL so a down store is asked once a
// minute, not once a request. The lock is held across the count, so a burst
// after expiry queries once.
func (c *statsCache) read(ctx context.Context, store Store, now func() time.Time) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	at := now()
	if c.body != nil && at.Sub(c.at) < statsTTL {
		return c.body, nil
	}
	total, active, err := store.CountTenants(ctx)
	if err != nil {
		if c.body != nil {
			c.at = at
			return c.body, nil
		}
		return nil, err
	}
	body, err := json.Marshal(struct {
		Signups       int64  `json:"signups"`
		ActiveTenants int64  `json:"active_tenants"`
		UpdatedAt     string `json:"updated_at"`
	}{total, active, at.UTC().Format(time.RFC3339)})
	if err != nil {
		return nil, err
	}
	c.at, c.body = at, append(body, '\n')
	return c.body, nil
}

// googleFrontEnds are the source ranges of Google's external Application Load
// Balancer proxies. Nothing else can arrive from them, so only from them is
// X-Forwarded-For believed.
var googleFrontEnds = []*net.IPNet{
	{IP: net.IPv4(35, 191, 0, 0), Mask: net.CIDRMask(16, 32)},
	{IP: net.IPv4(130, 211, 0, 0), Mask: net.CIDRMask(22, 32)},
}

// clientAddress is who a per-address limit counts against. Behind Google's
// balancer the peer is a proxy shared by every caller, and the balancer
// appends "<client>, <balancer>" to whatever X-Forwarded-For the client sent,
// so the second entry from the end is the one the client cannot forge.
func clientAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer == nil || !(googleFrontEnds[0].Contains(peer) || googleFrontEnds[1].Contains(peer)) {
		return host
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	if len(hops) < 2 {
		return host
	}
	if client := net.ParseIP(strings.TrimSpace(hops[len(hops)-2])); client != nil {
		return client.String()
	}
	return host
}

func (h *handler) signup(w http.ResponseWriter, r *http.Request) {
	if !h.signups.permit(clientAddress(r)) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many signups from this address"})
		return
	}
	var body struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if !decode(w, r, &body) {
		return
	}
	result, err := h.service.Signup(r.Context(), SignupRequest{Email: body.Email, Name: body.Name})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"tenant":       result.Tenant.ID,
		"status":       result.Tenant.Status,
		"api_key":      result.APIKey,
		"key_id":       result.Key.ID,
		"checkout_url": result.Checkout.URL,
		"note":         "The API key is shown once. Store it now; it cannot be recovered, only revoked and replaced.",
	})
}

func (h *handler) webhook(w http.ResponseWriter, r *http.Request) {
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, largestBody))
	if err != nil {
		writeError(w, errors.Join(ErrInvalid, err))
		return
	}
	event, err := VerifyWebhook(payload, r.Header.Get("Stripe-Signature"), h.service.webhookSecret, h.service.now())
	if err != nil {
		// The compliance module alerts on this event: a burst of them is
		// somebody probing the endpoint or a webhook secret out of step.
		h.service.log.WarnContext(r.Context(), "stripe webhook signature rejected",
			"event", "stripe_webhook_signature_rejected", "remote", clientAddress(r))
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "signature"})
		return
	}
	// A failure answers 500, which Stripe retries with backoff for three days.
	// Every handler is idempotent, so the retry is safe.
	if err := h.service.HandleStripeEvent(r.Context(), event); err != nil {
		h.service.log.ErrorContext(r.Context(), "stripe webhook failed", "event", event.ID, "type", event.Type, "error", err)
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"received": true})
}

type tenantView struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Email          string    `json:"email"`
	Status         Status    `json:"status"`
	HasCard        bool      `json:"has_card"`
	AutoTopUp      autoView  `json:"auto_top_up"`
	LastTopUpError string    `json:"last_top_up_error,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

type autoView struct {
	Enabled bool  `json:"enabled"`
	Below   money `json:"below"`
	Amount  money `json:"amount"`
}

type entryView struct {
	Kind      EntryKind `json:"kind"`
	Amount    money     `json:"amount"`
	Capped    bool      `json:"capped,omitempty"`
	Reference string    `json:"reference,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	At        time.Time `json:"at"`
}

func viewTenant(t Tenant) tenantView {
	return tenantView{
		ID: t.ID, Name: t.Name, Email: t.Email, Status: t.Status,
		HasCard:        t.PaymentMethod != "",
		AutoTopUp:      autoView{Enabled: t.AutoTopUp.Enabled, Below: moneyOf(t.AutoTopUp.Below), Amount: moneyOf(t.AutoTopUp.Amount)},
		LastTopUpError: t.LastTopUpError,
		CreatedAt:      t.CreatedAt,
	}
}

func (h *handler) summary(w http.ResponseWriter, r *http.Request, key Key) {
	summary, err := h.service.Summary(r.Context(), key.Tenant)
	if err != nil {
		writeError(w, err)
		return
	}
	entries := make([]entryView, 0, len(summary.Recent))
	for _, entry := range summary.Recent {
		// Cost is deliberately absent: what MOQOM pays Google is not the
		// customer's business.
		entries = append(entries, entryView{
			Kind: entry.Kind, Amount: moneyOf(entry.Amount), Capped: entry.Capped,
			Reference: entry.Reference, Detail: entry.Detail, At: entry.At,
		})
	}
	body := map[string]any{
		"tenant":   viewTenant(summary.Tenant),
		"balance":  moneyOf(summary.Balance),
		"admitted": true,
		"recent":   entries,
	}
	if err := h.service.Admitted(r.Context(), key.Tenant); err != nil {
		body["admitted"], body["refusal"] = false, refusal(err)
	}
	for name, value := range viewSpend(summary.Tenant, summary.Spend) {
		body[name] = value
	}
	writeJSON(w, http.StatusOK, body)
}

// refusal is why sessions are not being admitted, without leaking a store
// error.
func refusal(err error) string {
	for _, known := range []error{ErrPaymentRequired, ErrInsufficientCredit, ErrSpendLimitReached, ErrSuspended} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	return "internal error"
}

type alertsView struct {
	Enabled bool `json:"enabled"`
	// Default is true when the thresholds are the percentages of the limit.
	Default    bool    `json:"default"`
	Thresholds []money `json:"thresholds"`
}

type firedView struct {
	Threshold money     `json:"threshold"`
	At        time.Time `json:"at"`
}

func viewSpend(t Tenant, spend MonthSpend) map[string]any {
	thresholds := make([]money, 0, len(spend.Thresholds))
	for _, threshold := range spend.Thresholds {
		thresholds = append(thresholds, moneyOf(threshold))
	}
	fired := make([]firedView, 0, len(spend.Fired))
	for _, alert := range spend.Fired {
		fired = append(fired, firedView{Threshold: moneyOf(alert.Threshold), At: alert.At})
	}
	var limit *money
	if t.Spending.Limit > 0 {
		value := moneyOf(t.Spending.Limit)
		limit = &value
	}
	return map[string]any{
		"month":         spend.Month.Format("2006-01"),
		"month_spend":   moneyOf(spend.Spent),
		"spend_limit":   limit,
		"limit_reached": t.Spending.Limit > 0 && spend.Spent >= t.Spending.Limit,
		"alerts": alertsView{
			Enabled:    !t.Spending.AlertsOff,
			Default:    !t.Spending.AlertsOff && len(t.Spending.Alerts) == 0,
			Thresholds: thresholds,
		},
		"fired": fired,
	}
}

func (h *handler) writeSpend(w http.ResponseWriter, r *http.Request, tenant Tenant) {
	spend, err := h.service.Spend(r.Context(), tenant)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, viewSpend(tenant, spend))
}

func (h *handler) spendLimit(w http.ResponseWriter, r *http.Request, key Key) {
	var body struct {
		Enabled      bool  `json:"enabled"`
		MonthlyCents int64 `json:"monthly_cents"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !body.Enabled && body.MonthlyCents != 0 {
		writeError(w, errors.Join(ErrInvalid, errors.New("monthly_cents must be 0 or absent when enabled is false")))
		return
	}
	if body.Enabled && body.MonthlyCents <= 0 {
		writeError(w, errors.Join(ErrInvalid, errors.New("an enabled spend limit needs monthly_cents")))
		return
	}
	if body.MonthlyCents > MaxSpendAmount.Cents() {
		writeError(w, errors.Join(ErrInvalid, errors.New("monthly_cents is above the largest spend limit")))
		return
	}
	tenant, err := h.service.SetSpendLimit(r.Context(), key.Tenant, CentsToMicros(body.MonthlyCents))
	if err != nil {
		writeError(w, err)
		return
	}
	h.writeSpend(w, r, tenant)
}

func (h *handler) spendAlerts(w http.ResponseWriter, r *http.Request, key Key) {
	var body struct {
		Enabled         bool    `json:"enabled"`
		ThresholdsCents []int64 `json:"thresholds_cents"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !body.Enabled && len(body.ThresholdsCents) > 0 {
		writeError(w, errors.Join(ErrInvalid, errors.New("thresholds_cents must be empty when enabled is false")))
		return
	}
	if len(body.ThresholdsCents) > mostSpendAlerts {
		writeError(w, errors.Join(ErrInvalid, errors.New("at most 10 spend alerts")))
		return
	}
	thresholds := make([]Micros, 0, len(body.ThresholdsCents))
	for _, cents := range body.ThresholdsCents {
		if cents <= 0 || cents > MaxSpendAmount.Cents() {
			writeError(w, errors.Join(ErrInvalid, errors.New("each threshold must be between $1.00 and $1,000,000.00")))
			return
		}
		thresholds = append(thresholds, CentsToMicros(cents))
	}
	tenant, err := h.service.SetSpendAlerts(r.Context(), key.Tenant, body.Enabled, thresholds)
	if err != nil {
		writeError(w, err)
		return
	}
	h.writeSpend(w, r, tenant)
}

func (h *handler) checkout(w http.ResponseWriter, r *http.Request, key Key) {
	var body struct {
		AmountCents int64 `json:"amount_cents"`
	}
	if !decode(w, r, &body) {
		return
	}
	checkout, err := h.service.Checkout(r.Context(), key.Tenant, CentsToMicros(body.AmountCents))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"checkout_url": checkout.URL})
}

func (h *handler) portal(w http.ResponseWriter, r *http.Request, key Key) {
	url, err := h.service.Portal(r.Context(), key.Tenant)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"portal_url": url})
}

func (h *handler) autoTopUp(w http.ResponseWriter, r *http.Request, key Key) {
	var body struct {
		Enabled     bool  `json:"enabled"`
		BelowCents  int64 `json:"below_cents"`
		AmountCents int64 `json:"amount_cents"`
	}
	if !decode(w, r, &body) {
		return
	}
	tenant, err := h.service.SetAutoTopUp(r.Context(), key.Tenant, AutoTopUp{
		Enabled: body.Enabled,
		Below:   CentsToMicros(body.BelowCents),
		Amount:  CentsToMicros(body.AmountCents),
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, viewTenant(tenant))
}

type keyView struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Kind      KeyKind    `json:"kind"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	RetiresAt *time.Time `json:"retires_at,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Expired   bool       `json:"expired,omitempty"`
	// AllowedNetworks is empty for a key that works from anywhere.
	AllowedNetworks []string `json:"allowed_networks,omitempty"`
	// RateLimitPerMinute is the effective limit per replica, absent for none.
	RateLimitPerMinute int `json:"rate_limit_per_minute,omitempty"`
}

func viewKey(k Key) keyView { return viewKeyAt(k, time.Now()) }

func viewKeyAt(k Key, now time.Time) keyView {
	view := keyView{ID: k.ID, Name: k.Name, Kind: k.Kind, CreatedAt: k.CreatedAt,
		Expired: k.ExpiredAsOf(now), RateLimitPerMinute: k.Limit()}
	at := k.RevokedAt
	switch {
	case k.RevokedAsOf(now):
		view.RevokedAt = &at
	case k.Retiring(now):
		view.RetiresAt = &at
	}
	if !k.ExpiresAt.IsZero() {
		expires := k.ExpiresAt
		view.ExpiresAt = &expires
	}
	for _, network := range k.AllowedNetworks {
		view.AllowedNetworks = append(view.AllowedNetworks, network.String())
	}
	return view
}

func (h *handler) listKeys(w http.ResponseWriter, r *http.Request, key Key) {
	keys, err := h.service.ListKeys(r.Context(), key.Tenant)
	if err != nil {
		writeError(w, err)
		return
	}
	views := make([]keyView, 0, len(keys))
	for _, k := range keys {
		views = append(views, viewKeyAt(k, h.service.now()))
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": views})
}

func (h *handler) issueKey(w http.ResponseWriter, r *http.Request, key Key) {
	var body struct {
		Name               string     `json:"name"`
		Kind               KeyKind    `json:"kind"`
		ExpiresAt          *time.Time `json:"expires_at"`
		AllowedNetworks    []string   `json:"allowed_networks"`
		RateLimitPerMinute int        `json:"rate_limit_per_minute"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Kind == "" {
		body.Kind = KeyBackend
	}
	networks, err := ParseNetworks(body.AllowedNetworks)
	if err != nil {
		writeError(w, err)
		return
	}
	spec := KeySpec{Name: body.Name, Kind: body.Kind, AllowedNetworks: networks, RateLimit: body.RateLimitPerMinute}
	if body.ExpiresAt != nil {
		spec.ExpiresAt = *body.ExpiresAt
	}
	issued, plaintext, err := h.service.IssueKeyWith(r.Context(), key.Tenant, spec)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"key": viewKeyAt(issued, h.service.now()), "api_key": plaintext})
}

func (h *handler) revokeKey(w http.ResponseWriter, r *http.Request, key Key) {
	id := r.PathValue("id")
	if id == key.ID {
		// Revoking the key the request arrived on would lock a single-key
		// tenant out of its own account.
		writeError(w, errors.Join(ErrInvalid, errors.New("issue a replacement and revoke this key with it")))
		return
	}
	confirmed := r.URL.Query().Get("confirm") == confirmStopService
	if err := h.service.RevokeKey(r.Context(), key.Tenant, id, confirmed); err != nil {
		if errors.Is(err, ErrWouldStopService) {
			effect := "Revoking it stops all video: your backend can no longer mint tokens, so new sessions are " +
				"refused at once and live ones end within 15 minutes, when their tokens can't renew."
			if strings.Contains(err.Error(), string(KeyRelay)) {
				effect = "Revoking it stops your self-hosted relays reporting usage."
			}
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "This is your last working key of its kind. " + effect + " Rotate instead (POST /v1/keys/" + id +
					"/rotate) to add a replacement before the old key stops, or repeat with ?confirm=" +
					confirmStopService + " to revoke it anyway.",
				"code": "would_stop_service",
			})
			return
		}
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

const confirmStopService = "stop-service"

func (h *handler) rotateKey(w http.ResponseWriter, r *http.Request, key Key) {
	var body struct {
		OverlapSeconds *int64 `json:"overlap_seconds"`
	}
	if r.ContentLength != 0 && !decode(w, r, &body) {
		return
	}
	overlap := DefaultRotationOverlap
	if body.OverlapSeconds != nil {
		overlap = time.Duration(*body.OverlapSeconds) * time.Second
	}
	issued, plaintext, retires, err := h.service.RotateKey(r.Context(), key.Tenant, r.PathValue("id"), overlap)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"key":             viewKeyAt(issued, h.service.now()),
		"api_key":         plaintext,
		"old_key_retires": retires,
	})
}

type usageBody struct {
	// Hosting is "cloudflare" for usage on Cloudflare's relays, and absent for
	// the reporter's own relay. Only a tenant's relay key may set it.
	Hosting string `json:"hosting"`
	Records []struct {
		ID           string `json:"id"`
		Tenant       string `json:"tenant"`
		Room         string `json:"room"`
		Participant  string `json:"participant"`
		Role         Role   `json:"role"`
		Tier         Tier   `json:"tier"`
		Seconds      int64  `json:"seconds"`
		EgressBytes  int64  `json:"egress_bytes"`
		IngressBytes int64  `json:"ingress_bytes"`
		// Absent from relays that predate direct tickets, which is zero.
		DirectSeconds int64 `json:"direct_seconds"`
	} `json:"records"`
}

const mostRecordsPerReport = 5000

func (h *handler) reporter(ctx context.Context, credential, address string) (Reporter, bool) {
	if _, _, isKey := ParseKey(credential); isKey {
		key, err := h.service.keys.Authorize(ctx, credential, Use{Operation: usageRoute, Address: address})
		return Reporter{Tenant: key.Tenant, Hosting: HostingSelf}, err == nil
	}
	if h.operators == nil {
		return Reporter{}, false
	}
	account, err := h.operators.Lookup(ctx, credential)
	if err != nil || !account.IsRelay || account.Tenant != "" {
		return Reporter{}, false
	}
	return Reporter{Hosting: HostingCloud}, true
}

func (h *handler) usage(w http.ResponseWriter, r *http.Request) {
	reporter, ok := h.reporter(r.Context(), bearer(r), clientAddress(r))
	if !ok {
		writeError(w, ErrInvalidKey)
		return
	}
	var body usageBody
	if !decode(w, r, &body) {
		return
	}
	switch {
	case body.Hosting == "":
	case body.Hosting == string(HostingCloudflare) && reporter.Tenant != "":
		reporter.Hosting = HostingCloudflare
	default:
		writeError(w, errors.Join(ErrInvalid, errors.New(`hosting is "cloudflare", from a tenant's relay key, or absent`)))
		return
	}
	if len(body.Records) > mostRecordsPerReport {
		writeError(w, errors.Join(ErrInvalid, errors.New("at most 5000 records per report")))
		return
	}
	records := make([]UsageRecord, 0, len(body.Records))
	for _, record := range body.Records {
		records = append(records, UsageRecord{
			ID: record.ID, Tenant: record.Tenant, Room: record.Room, Participant: record.Participant,
			Role: record.Role, Tier: record.Tier, Seconds: record.Seconds,
			EgressBytes: record.EgressBytes, IngressBytes: record.IngressBytes,
			DirectSeconds: record.DirectSeconds,
		})
	}
	result, err := h.service.RecordUsage(r.Context(), reporter, records)
	if err != nil {
		writeError(w, err)
		return
	}
	response := map[string]any{
		"charged": result.Charged, "duplicates": result.Duplicates,
		"unbilled": result.Unbilled, "total": moneyOf(result.Total),
	}
	if reporter.Tenant != "" {
		// A self-hosted relay is told whether to keep admitting sessions, so the
		// gate holds on infrastructure MOQOM does not run.
		response["admitted"] = h.service.Admitted(r.Context(), reporter.Tenant) == nil
	}
	writeJSON(w, http.StatusOK, response)
}

// CORS lets the listed browser origins call the billing API: the signup form on
// moqom.cloud and a customer's own console. Credentials are bearer headers, not
// cookies, so no Allow-Credentials and no ambient authority for a hostile page.
// The public stats route is read-only and keyless, so its preflight offers GET
// and no request headers at all.
func CORS(next http.Handler, origins []string) http.Handler {
	allowed := map[string]bool{}
	for _, origin := range origins {
		if origin = strings.TrimSpace(origin); origin != "" {
			allowed[origin] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && allowed[origin] && r.URL.Path != "/v1/stripe/webhook" && r.URL.Path != "/v1/usage" &&
			r.URL.Path != GitHubSecretScanningPath {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
			if r.Method == http.MethodOptions && r.URL.Path == PublicStatsPath {
				w.Header().Set("Access-Control-Allow-Methods", "GET")
				w.Header().Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if r.Method == http.MethodOptions {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				w.Header().Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

const sweepAbove = 4096

// windowLimiter allows per events per window for each key.
type windowLimiter struct {
	mu     sync.Mutex
	per    int
	window time.Duration
	now    func() time.Time
	seen   map[string][]time.Time
}

func (l *windowLimiter) permit(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = map[string][]time.Time{}
	}
	now := l.now()
	if len(l.seen) >= sweepAbove {
		// Addresses that stopped calling would otherwise be held forever.
		for other, times := range l.seen {
			if len(times) == 0 || now.Sub(times[len(times)-1]) >= l.window {
				delete(l.seen, other)
			}
		}
	}
	recent := l.seen[key][:0]
	for _, at := range l.seen[key] {
		if now.Sub(at) < l.window {
			recent = append(recent, at)
		}
	}
	if len(recent) >= l.per {
		l.seen[key] = recent
		return false
	}
	l.seen[key] = append(recent, now)
	return true
}
