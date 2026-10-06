package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The operator API: MOQOM staff looking across tenants.
//
//	GET  /v1/operator/whoami                         the signed-in operator
//	GET  /v1/operator/tenants                        every tenant, with balance, month spend, last activity
//	GET  /v1/operator/tenants/{id}                   one tenant, its ledger and key metadata
//	POST /v1/operator/tenants/{id}/status            suspend or reactivate, with a reason
//	POST /v1/operator/tenants/{id}/adjustments       credit or debit with a reason, idempotent
//	GET  /v1/operator/signups?days=90                signups per UTC day
//	GET  /v1/operator/audit?tenant=&limit=           the operator audit log
//
// Authenticated by a Google ID token in the Authorization header (see
// operator_auth.go), never by a tenant key. Every authenticated request is
// written to the audit log before it is served, and is refused if it cannot
// be: an action nobody can account for is not one this API takes.

// OperatorStore is what the operator API needs beyond Store. Separate so a
// store without it still runs billing; the operator API refuses to start on
// one.
type OperatorStore interface {
	// ListTenantOverviews is every tenant with its balance, its usage since
	// month, and its latest ledger entry.
	ListTenantOverviews(ctx context.Context, month time.Time) ([]TenantOverview, error)
	AppendAudit(ctx context.Context, entry AuditEntry) error
	// ListAudit is the newest entries first, for one tenant or, with tenant
	// empty, all of them.
	ListAudit(ctx context.Context, tenant string, limit int) ([]AuditEntry, error)
}

// TenantOverview is one row of the operator's tenant list.
type TenantOverview struct {
	Tenant     Tenant
	Balance    Micros
	MonthSpend Micros
	// LastActivity is the newest ledger entry, zero for a tenant with none.
	LastActivity time.Time
}

// AuditEntry is one thing an operator did or looked at.
type AuditEntry struct {
	ID       string
	At       time.Time
	Operator string
	Subject  string
	Address  string
	Action   string
	Tenant   string
	Detail   string
	Outcome  string
}

// Bounds on a manual adjustment.
const (
	MaxAdjustment     = 10_000 * Dollar
	shortestReason    = 4
	longestReason     = 500
	operatorAuditPage = 200
)

var idempotencyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,100}$`)

// ErrIdempotencyMismatch is an idempotency key reused for a different
// adjustment. Answering with the first one would hide a mistake; applying the
// second would break the promise the key makes.
var ErrIdempotencyMismatch = errors.Join(ErrAlreadyExists, errors.New("that idempotency key was used for a different adjustment"))

// AdjustCredit adds amount, which may be negative, to a tenant's balance as a
// ledger entry of kind adjustment. The reason is recorded in the entry and is
// shown to the tenant in its ledger. Retrying with the same idempotency key
// returns the first entry and false.
func (s *Service) AdjustCredit(ctx context.Context, tenant string, amount Micros, reason, idempotency string) (Entry, bool, error) {
	reason = strings.TrimSpace(reason)
	switch {
	case amount == 0 || amount%Cent != 0:
		return Entry{}, false, fmt.Errorf("%w: an adjustment is a non-zero number of whole cents", ErrInvalid)
	case amount > MaxAdjustment || amount < -MaxAdjustment:
		return Entry{}, false, fmt.Errorf("%w: an adjustment is at most %s either way", ErrInvalid, MaxAdjustment)
	case len(reason) < shortestReason || len(reason) > longestReason:
		return Entry{}, false, fmt.Errorf("%w: a reason of %d to %d characters is required", ErrInvalid, shortestReason, longestReason)
	case !idempotencyPattern.MatchString(idempotency):
		return Entry{}, false, fmt.Errorf("%w: idempotency_key must be 8 to 100 letters, digits, - or _", ErrInvalid)
	}
	if _, err := s.store.GetTenant(ctx, tenant); err != nil {
		return Entry{}, false, err
	}
	id, err := s.randomHex(12)
	if err != nil {
		return Entry{}, false, err
	}
	detail, _ := json.Marshal(map[string]string{"reason": reason})
	entry, created, err := s.store.AppendEntry(ctx, Entry{
		ID:             id,
		Tenant:         tenant,
		Kind:           KindAdjustment,
		Amount:         amount,
		IdempotencyKey: "operator-adjustment:" + tenant + ":" + idempotency,
		Reference:      "adj_" + id,
		Detail:         string(detail),
		At:             s.now(),
	})
	if err != nil {
		return Entry{}, false, err
	}
	if !created && (entry.Tenant != tenant || entry.Kind != KindAdjustment || entry.Amount != amount || entry.Detail != string(detail)) {
		return Entry{}, false, ErrIdempotencyMismatch
	}
	return entry, created, nil
}

// OperatorVerifier turns a presented credential into an operator.
type OperatorVerifier interface {
	Verify(ctx context.Context, credential string) (Operator, error)
}

// OperatorOptions configures the operator API.
type OperatorOptions struct {
	Verifier OperatorVerifier
	// Origins may call it from a browser: the admin console's, and nothing
	// on the public site.
	Origins []string
}

type operatorHandler struct {
	service   *Service
	store     OperatorStore
	verifier  OperatorVerifier
	addresses *windowLimiter
	changes   *windowLimiter
}

// OperatorHandler serves /v1/operator/*, with its own CORS.
func (s *Service) OperatorHandler(options OperatorOptions) (http.Handler, error) {
	if options.Verifier == nil {
		return nil, errors.New("the operator API needs a verifier")
	}
	store, ok := s.store.(OperatorStore)
	if !ok {
		return nil, errors.New("the operator API needs a store that keeps an audit log")
	}
	h := &operatorHandler{
		service: s, store: store, verifier: options.Verifier,
		addresses: &windowLimiter{per: 120, window: time.Minute, now: s.now},
		changes:   &windowLimiter{per: 30, window: time.Hour, now: s.now},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/operator/whoami", h.authed("whoami", h.whoami))
	mux.HandleFunc("GET /v1/operator/tenants", h.authed("list_tenants", h.listTenants))
	mux.HandleFunc("GET /v1/operator/tenants/{id}", h.authed("view_tenant", h.viewTenant))
	mux.HandleFunc("POST /v1/operator/tenants/{id}/status", h.authed("set_status", h.setStatus))
	mux.HandleFunc("POST /v1/operator/tenants/{id}/adjustments", h.authed("adjust_credit", h.adjust))
	mux.HandleFunc("GET /v1/operator/signups", h.authed("signups", h.signups))
	mux.HandleFunc("GET /v1/operator/audit", h.authed("read_audit", h.audit))
	mux.HandleFunc("/v1/operator/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	})
	return operatorCORS(mux, options.Origins), nil
}

// operatorCORS answers only the admin console's origin, which is never one of
// the public site's.
func operatorCORS(next http.Handler, origins []string) http.Handler {
	allowed := map[string]bool{}
	for _, origin := range origins {
		if origin = strings.TrimSpace(origin); origin != "" {
			allowed[origin] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		origin := r.Header.Get("Origin")
		if origin != "" && allowed[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
			if r.Method == http.MethodOptions {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				w.Header().Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

type operatorRequest struct {
	operator Operator
	address  string
	action   string
	tenant   string
}

func (h *operatorHandler) authed(action string, next func(http.ResponseWriter, *http.Request, operatorRequest)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log := h.service.log
		address := clientAddress(r)
		if !h.addresses.permit(address) {
			w.Header().Set("Retry-After", "60")
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many requests from this address"})
			return
		}
		credential := bearer(r)
		operator, err := h.verifier.Verify(r.Context(), credential)
		if err != nil {
			attrs := []any{"event", "operator_refused", "action", action, "remote", address, "path", r.URL.Path}
			if _, _, isKey := ParseKey(credential); isKey {
				attrs = append(attrs, "presented", "tenant_api_key")
			}
			log.WarnContext(r.Context(), "operator request refused", attrs...)
			w.Header().Set("WWW-Authenticate", `Bearer realm="moqom-operator"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "an operator ID token is required"})
			return
		}
		request := operatorRequest{operator: operator, address: address, action: action, tenant: r.PathValue("id")}
		if r.Method != http.MethodGet && !h.changes.permit(operator.Email) {
			w.Header().Set("Retry-After", "60")
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many changes by this operator in the last hour"})
			return
		}
		if err := h.record(r.Context(), request, r.URL.RawQuery, "requested"); err != nil {
			log.ErrorContext(r.Context(), "operator audit write failed; request refused", "action", action, "error", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the audit log could not be written, so nothing was done"})
			return
		}
		next(w, r, request)
	}
}

// record writes an audit entry and logs it, so the trail is in both the
// database and Cloud Logging.
func (h *operatorHandler) record(ctx context.Context, request operatorRequest, detail, outcome string) error {
	id, err := h.service.randomHex(12)
	if err != nil {
		return err
	}
	entry := AuditEntry{
		ID: id, At: h.service.now(), Operator: request.operator.Email, Subject: request.operator.Subject,
		Address: request.address, Action: request.action, Tenant: request.tenant, Detail: detail, Outcome: outcome,
	}
	h.service.log.InfoContext(ctx, "operator action", "event", "operator_action", "audit", id,
		"operator", entry.Operator, "action", entry.Action, "tenant", entry.Tenant,
		"remote", entry.Address, "detail", entry.Detail, "outcome", entry.Outcome)
	return h.store.AppendAudit(ctx, entry)
}

// finish records how a change ended. A failure to write it is logged rather
// than reported, because the change has happened and the request's own entry
// already says it was asked for.
func (h *operatorHandler) finish(ctx context.Context, request operatorRequest, detail string, err error) {
	outcome := "ok"
	if err != nil {
		outcome = "failed: " + refusalText(err)
	}
	if werr := h.record(ctx, request, detail, outcome); werr != nil {
		h.service.log.ErrorContext(ctx, "operator outcome not audited", "action", request.action, "tenant", request.tenant, "error", werr)
	}
}

func refusalText(err error) string {
	for _, known := range []error{ErrInvalid, ErrNotFound, ErrTransition, ErrAlreadyExists} {
		if errors.Is(err, known) {
			return err.Error()
		}
	}
	return "internal error"
}

func (h *operatorHandler) whoami(w http.ResponseWriter, _ *http.Request, request operatorRequest) {
	writeJSON(w, http.StatusOK, map[string]string{"email": request.operator.Email})
}

type operatorTenantRow struct {
	tenantView
	Balance      money      `json:"balance"`
	MonthSpend   money      `json:"month_spend"`
	SpendLimit   *money     `json:"spend_limit"`
	LastActivity *time.Time `json:"last_activity"`
	Exempt       bool       `json:"exempt"`
}

func (h *operatorHandler) row(overview TenantOverview) operatorTenantRow {
	row := operatorTenantRow{
		tenantView: viewTenant(overview.Tenant),
		Balance:    moneyOf(overview.Balance),
		MonthSpend: moneyOf(overview.MonthSpend),
		Exempt:     h.service.exempt[overview.Tenant.ID],
	}
	if overview.Tenant.Spending.Limit > 0 {
		limit := moneyOf(overview.Tenant.Spending.Limit)
		row.SpendLimit = &limit
	}
	if !overview.LastActivity.IsZero() {
		at := overview.LastActivity
		row.LastActivity = &at
	}
	return row
}

func (h *operatorHandler) listTenants(w http.ResponseWriter, r *http.Request, _ operatorRequest) {
	month := MonthOf(h.service.now())
	overviews, err := h.store.ListTenantOverviews(r.Context(), month)
	if err != nil {
		writeError(w, err)
		return
	}
	sort.Slice(overviews, func(i, j int) bool { return overviews[i].Tenant.CreatedAt.After(overviews[j].Tenant.CreatedAt) })
	rows := make([]operatorTenantRow, 0, len(overviews))
	for _, overview := range overviews {
		rows = append(rows, h.row(overview))
	}
	writeJSON(w, http.StatusOK, map[string]any{"month": month.Format("2006-01"), "tenants": rows})
}

type operatorEntryView struct {
	ID string `json:"id"`
	entryView
	// Cost is MOQOM's, which the tenant never sees and the operator needs for
	// margin.
	Cost money `json:"cost"`
}

const operatorLedgerPage = 500

func (h *operatorHandler) viewTenant(w http.ResponseWriter, r *http.Request, request operatorRequest) {
	ctx := r.Context()
	summary, err := h.service.Summary(ctx, request.tenant)
	if err != nil {
		writeError(w, err)
		return
	}
	entries, err := h.service.store.ListEntries(ctx, request.tenant, operatorLedgerPage)
	if err != nil {
		writeError(w, err)
		return
	}
	keys, err := h.service.store.ListKeys(ctx, request.tenant)
	if err != nil {
		writeError(w, err)
		return
	}
	ledger := make([]operatorEntryView, 0, len(entries))
	for _, e := range entries {
		ledger = append(ledger, operatorEntryView{
			ID: e.ID,
			entryView: entryView{Kind: e.Kind, Amount: moneyOf(e.Amount), Capped: e.Capped,
				Reference: e.Reference, Detail: e.Detail, At: e.At},
			Cost: moneyOf(e.Cost),
		})
	}
	keyViews := make([]keyView, 0, len(keys))
	for _, k := range keys {
		keyViews = append(keyViews, viewKey(k))
	}
	body := map[string]any{
		"tenant":   viewTenant(summary.Tenant),
		"balance":  moneyOf(summary.Balance),
		"exempt":   h.service.exempt[request.tenant],
		"admitted": true,
		"ledger":   ledger,
		"keys":     keyViews,
	}
	if err := h.service.Admitted(ctx, request.tenant); err != nil {
		body["admitted"], body["refusal"] = false, refusal(err)
	}
	for name, value := range viewSpend(summary.Tenant, summary.Spend) {
		body[name] = value
	}
	writeJSON(w, http.StatusOK, body)
}

func (h *operatorHandler) setStatus(w http.ResponseWriter, r *http.Request, request operatorRequest) {
	var body struct {
		Status Status `json:"status"`
		Reason string `json:"reason"`
	}
	if !decode(w, r, &body) {
		h.finish(r.Context(), request, "", ErrInvalid)
		return
	}
	reason := strings.TrimSpace(body.Reason)
	detail, _ := json.Marshal(map[string]string{"status": string(body.Status), "reason": reason})
	fail := func(err error) {
		h.finish(r.Context(), request, string(detail), err)
		writeError(w, err)
	}
	if len(reason) < shortestReason || len(reason) > longestReason {
		fail(fmt.Errorf("%w: a reason of %d to %d characters is required", ErrInvalid, shortestReason, longestReason))
		return
	}
	current, err := h.service.store.GetTenant(r.Context(), request.tenant)
	if err != nil {
		fail(err)
		return
	}
	// Suspend and reactivate only. Activating a tenant that never paid, and
	// archiving or deleting one, are not console actions.
	switch {
	case body.Status == StatusSuspended && current.Status == StatusActive:
	case body.Status == StatusActive && current.Status == StatusSuspended:
	default:
		fail(fmt.Errorf("%w: %s to %s is not an operator action; suspend an active tenant or reactivate a suspended one",
			ErrTransition, current.Status, body.Status))
		return
	}
	tenant, err := h.service.SetStatus(r.Context(), request.tenant, body.Status)
	h.finish(r.Context(), request, string(detail), err)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, viewTenant(tenant))
}

func (h *operatorHandler) adjust(w http.ResponseWriter, r *http.Request, request operatorRequest) {
	var body struct {
		AmountCents    int64  `json:"amount_cents"`
		Reason         string `json:"reason"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if !decode(w, r, &body) {
		h.finish(r.Context(), request, "", ErrInvalid)
		return
	}
	detail, _ := json.Marshal(map[string]any{
		"amount_cents": body.AmountCents, "reason": strings.TrimSpace(body.Reason), "idempotency_key": body.IdempotencyKey,
	})
	var entry Entry
	var created bool
	var err error
	if body.AmountCents > MaxAdjustment.Cents() || body.AmountCents < -MaxAdjustment.Cents() {
		err = fmt.Errorf("%w: an adjustment is at most %s either way", ErrInvalid, MaxAdjustment)
	} else {
		entry, created, err = h.service.AdjustCredit(r.Context(), request.tenant, CentsToMicros(body.AmountCents), body.Reason, body.IdempotencyKey)
	}
	outcome := string(detail)
	if err == nil && !created {
		outcome += " (replay of " + entry.Reference + ")"
	}
	h.finish(r.Context(), request, outcome, err)
	if err != nil {
		writeError(w, err)
		return
	}
	balance, err := h.service.store.Balance(r.Context(), request.tenant)
	if err != nil {
		writeError(w, err)
		return
	}
	code := http.StatusCreated
	if !created {
		code = http.StatusOK
	}
	writeJSON(w, code, map[string]any{
		"entry": operatorEntryView{
			ID: entry.ID,
			entryView: entryView{Kind: entry.Kind, Amount: moneyOf(entry.Amount), Reference: entry.Reference,
				Detail: entry.Detail, At: entry.At},
			Cost: moneyOf(entry.Cost),
		},
		"duplicate": !created,
		"balance":   moneyOf(balance),
	})
}

func (h *operatorHandler) signups(w http.ResponseWriter, r *http.Request, _ operatorRequest) {
	days := 90
	if raw := r.URL.Query().Get("days"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 730 {
			writeError(w, fmt.Errorf("%w: days must be 1 to 730", ErrInvalid))
			return
		}
		days = parsed
	}
	overviews, err := h.store.ListTenantOverviews(r.Context(), MonthOf(h.service.now()))
	if err != nil {
		writeError(w, err)
		return
	}
	today := h.service.now().UTC().Truncate(24 * time.Hour)
	first := today.AddDate(0, 0, -(days - 1))
	counts := make([]int, days)
	total, active := 0, 0
	for _, overview := range overviews {
		total++
		if overview.Tenant.Status == StatusActive {
			active++
		}
		day := overview.Tenant.CreatedAt.UTC().Truncate(24 * time.Hour)
		if day.Before(first) || day.After(today) {
			continue
		}
		counts[int(day.Sub(first)/(24*time.Hour))]++
	}
	type dayCount struct {
		Date    string `json:"date"`
		Signups int    `json:"signups"`
	}
	series := make([]dayCount, days)
	for i := range counts {
		series[i] = dayCount{Date: first.AddDate(0, 0, i).Format("2006-01-02"), Signups: counts[i]}
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": series, "total": total, "active": active})
}

type auditView struct {
	ID       string    `json:"id"`
	At       time.Time `json:"at"`
	Operator string    `json:"operator"`
	Address  string    `json:"address"`
	Action   string    `json:"action"`
	Tenant   string    `json:"tenant,omitempty"`
	Detail   string    `json:"detail,omitempty"`
	Outcome  string    `json:"outcome"`
}

func (h *operatorHandler) audit(w http.ResponseWriter, r *http.Request, _ operatorRequest) {
	limit := operatorAuditPage
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 1000 {
			writeError(w, fmt.Errorf("%w: limit must be 1 to 1000", ErrInvalid))
			return
		}
		limit = parsed
	}
	entries, err := h.store.ListAudit(r.Context(), r.URL.Query().Get("tenant"), limit)
	if err != nil {
		writeError(w, err)
		return
	}
	views := make([]auditView, 0, len(entries))
	for _, e := range entries {
		views = append(views, auditView{ID: e.ID, At: e.At, Operator: e.Operator, Address: e.Address,
			Action: e.Action, Tenant: e.Tenant, Detail: e.Detail, Outcome: e.Outcome})
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": views})
}

// Memory as an OperatorStore.

var _ OperatorStore = (*Memory)(nil)

func (m *Memory) ListTenantOverviews(_ context.Context, month time.Time) ([]TenantOverview, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	byTenant := map[string]*TenantOverview{}
	overviews := make([]TenantOverview, 0, len(m.tenants))
	for _, tenant := range m.tenants {
		overviews = append(overviews, TenantOverview{Tenant: cloneTenant(tenant)})
	}
	for i := range overviews {
		byTenant[overviews[i].Tenant.ID] = &overviews[i]
	}
	for _, entry := range m.entries {
		overview := byTenant[entry.Tenant]
		if overview == nil {
			continue
		}
		overview.Balance += entry.Amount
		if entry.Kind == KindUsage && !entry.At.Before(month) {
			overview.MonthSpend -= entry.Amount
		}
		if entry.At.After(overview.LastActivity) {
			overview.LastActivity = entry.At
		}
	}
	return overviews, nil
}

func (m *Memory) AppendAudit(_ context.Context, entry AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audit = append(m.audit, entry)
	return nil
}

func (m *Memory) ListAudit(_ context.Context, tenant string, limit int) ([]AuditEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var found []AuditEntry
	for i := len(m.audit) - 1; i >= 0 && (limit <= 0 || len(found) < limit); i-- {
		if tenant == "" || m.audit[i].Tenant == tenant {
			found = append(found, m.audit[i])
		}
	}
	return found, nil
}
