package billing

import (
	"errors"
	"net/http"
	"strconv"
)

// Bounds on GET /v1/billing/ledger. The console charts usage from it and
// exports it as CSV; a tenant wanting everything ever uses the account export.
const (
	defaultLedgerPage = 1000
	largestLedgerPage = 5000
)

// ledger is the newest entries first, without MOQOM's cost.
func (h *handler) ledger(w http.ResponseWriter, r *http.Request, key Key) {
	limit := defaultLedgerPage
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > largestLedgerPage {
			writeError(w, errors.Join(ErrInvalid, errors.New("limit must be 1 to 5000")))
			return
		}
		limit = parsed
	}
	entries, err := h.service.store.ListEntries(r.Context(), key.Tenant, limit)
	if err != nil {
		writeError(w, err)
		return
	}
	views := make([]entryView, 0, len(entries))
	for _, e := range entries {
		views = append(views, entryView{
			Kind: e.Kind, Amount: moneyOf(e.Amount), Capped: e.Capped,
			Reference: e.Reference, Detail: e.Detail, At: e.At,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": views, "limit": limit, "complete": len(entries) < limit})
}
