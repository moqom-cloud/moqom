package billing

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

type keyUseView struct {
	KeyID     string    `json:"key_id"`
	Minute    time.Time `json:"minute"`
	Operation string    `json:"operation"`
	Address   string    `json:"address"`
	Outcome   string    `json:"outcome"`
	Detail    string    `json:"detail,omitempty"`
	Count     int64     `json:"count"`
	FirstAt   time.Time `json:"first_at"`
	LastAt    time.Time `json:"last_at"`
}

// keyAudit serves GET /v1/keys/{id}/audit and GET /v1/audit (optionally
// ?key=). Uses of a key with a wrong secret are included: they are the
// tenant's to see, since its key ID was being guessed at.
func (h *handler) keyAudit(w http.ResponseWriter, r *http.Request, key Key) {
	limit := keyAuditPage
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > largestKeyAudit {
			writeError(w, fmt.Errorf("%w: limit must be 1 to %d", ErrInvalid, largestKeyAudit))
			return
		}
		limit = parsed
	}
	id := r.PathValue("id")
	if id == "" {
		id = r.URL.Query().Get("key")
	}
	uses, err := h.service.KeyAudit(r.Context(), key.Tenant, id, limit)
	if err != nil {
		writeError(w, err)
		return
	}
	views := make([]keyUseView, 0, len(uses))
	for _, u := range uses {
		views = append(views, keyUseView{KeyID: u.Key, Minute: u.Minute, Operation: u.Operation,
			Address: u.Address, Outcome: u.Outcome, Detail: u.Detail, Count: u.Count,
			FirstAt: u.FirstAt, LastAt: u.LastAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": views})
}
