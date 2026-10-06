package billing

import (
	"context"
	"net/http"
	"time"
)

// AccountExport is everything MOQOM holds about a tenant as the controller of
// its account: the GDPR Art. 15 / 20 and CCPA right-to-know answer for the
// tenant itself. See docs/compliance/DSAR.md.
//
// Room, participant and moderation data are not here. MOQOM processes those
// on the tenant's behalf, and the tenant already reads them through the admin
// API with the same key; answering its own users' requests is the tenant's
// job as their controller, with MOQOM assisting.
type AccountExport struct {
	GeneratedAt time.Time    `json:"generated_at"`
	Tenant      tenantView   `json:"tenant"`
	Balance     money        `json:"balance"`
	Keys        []keyView    `json:"api_keys"`
	Ledger      []entryView  `json:"ledger"`
	Elsewhere   []heldByWhom `json:"held_elsewhere"`
}

type heldByWhom struct {
	Holder string `json:"holder"`
	What   string `json:"what"`
	How    string `json:"how_to_get_it"`
}

// Export assembles a tenant's account export. Key hashes and MOQOM's own cost
// of usage are left out: neither is the tenant's personal data, and the first
// is a credential verifier.
func (s *Service) Export(ctx context.Context, tenant string) (AccountExport, error) {
	record, err := s.store.GetTenant(ctx, tenant)
	if err != nil {
		return AccountExport{}, err
	}
	balance, err := s.store.Balance(ctx, tenant)
	if err != nil {
		return AccountExport{}, err
	}
	keys, err := s.store.ListKeys(ctx, tenant)
	if err != nil {
		return AccountExport{}, err
	}
	entries, err := s.store.ListEntries(ctx, tenant, 0)
	if err != nil {
		return AccountExport{}, err
	}
	export := AccountExport{
		GeneratedAt: s.now().UTC(),
		Tenant:      viewTenant(record),
		Balance:     moneyOf(balance),
		Keys:        make([]keyView, 0, len(keys)),
		Ledger:      make([]entryView, 0, len(entries)),
		Elsewhere: []heldByWhom{
			{
				Holder: "Stripe",
				What:   "card details, billing address, invoices and receipts",
				How:    "POST /v1/billing/portal opens Stripe's customer portal; MOQOM never receives card data",
			},
			{
				Holder: "MOQOM control plane (as your processor)",
				What:   "rooms, participants, moderation log and events in your tenant",
				How:    "the admin API, with this key",
			},
		},
	}
	for _, k := range keys {
		export.Keys = append(export.Keys, viewKey(k))
	}
	for _, e := range entries {
		export.Ledger = append(export.Ledger, entryView{
			Kind: e.Kind, Amount: moneyOf(e.Amount), Capped: e.Capped,
			Reference: e.Reference, Detail: e.Detail, At: e.At,
		})
	}
	return export, nil
}

func (h *handler) export(w http.ResponseWriter, r *http.Request, key Key) {
	export, err := h.service.Export(r.Context(), key.Tenant)
	if err != nil {
		writeError(w, err)
		return
	}
	// Logged so the request itself is on record: GDPR Art. 12 and CCPA
	// 1798.130 both expect a controller to show what it answered and when.
	h.service.log.InfoContext(r.Context(), "account export served",
		"event", "dsar_export", "tenant", key.Tenant, "key", key.ID,
		"ledger_entries", len(export.Ledger), "keys", len(export.Keys))
	w.Header().Set("Content-Disposition", `attachment; filename="moqom-account-export.json"`)
	writeJSON(w, http.StatusOK, export)
}
