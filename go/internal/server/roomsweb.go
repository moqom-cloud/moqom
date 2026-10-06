package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	adminpb "github.com/moqom-cloud/moqom-go/admin/adminpb"
	"github.com/moqom-cloud/moqom/go/internal/control"
	"github.com/moqom-cloud/moqom/go/internal/store"
)

// A read-only HTTP view of rooms and their events, for the console in a
// browser, which cannot speak gRPC. Two routes and nothing that changes state:
// moderation stays on the admin API, where every action names who it is on
// behalf of.
//
// Authenticated like the live map, and narrower: a bearer credential in the
// Authorization header that resolves to a tenant's account with owner standing.
// Relay credentials, accounts without owner standing and platform-operator
// accounts are all refused, and a tenant naming another in `?tenant=` is
// refused before anything is read.

const (
	// RoomsPath lists the tenant's rooms with their counts.
	RoomsPath = "/v1/rooms"
	// RoomEventsStreamPath streams the tenant's room events as server-sent
	// events, optionally for one room and resuming after a sequence.
	RoomEventsStreamPath = "/v1/rooms/events/stream"

	largestRoomsPage = 100
)

// RoomsOptions configures the rooms endpoints.
type RoomsOptions struct {
	// MaxStreams is how many event streams one tenant may hold open. Default 4.
	MaxStreams int
	// MaxStreamAge closes a stream after this long; the console resumes from
	// its last sequence. Default 15 min.
	MaxStreamAge time.Duration
	// Recheck is how often an open stream's credential is looked up again, so
	// revoking a key ends its streams. Default 1 min.
	Recheck time.Duration
	// Heartbeat is how often an idle stream sends a comment, so proxies do not
	// close it. Default 20 s.
	Heartbeat time.Duration
}

// RoomsHandler serves RoomsPath and RoomEventsStreamPath.
func (s *Server) RoomsHandler(options RoomsOptions) http.Handler {
	if options.MaxStreams <= 0 {
		options.MaxStreams = 4
	}
	if options.MaxStreamAge <= 0 {
		options.MaxStreamAge = 15 * time.Minute
	}
	if options.Recheck <= 0 {
		options.Recheck = time.Minute
	}
	if options.Heartbeat <= 0 {
		options.Heartbeat = 20 * time.Second
	}
	h := &roomsWeb{core: s.core, options: options, open: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+RoomsPath, h.list)
	mux.HandleFunc("GET "+RoomEventsStreamPath, h.stream)
	return mux
}

type roomsWeb struct {
	core    *core
	options RoomsOptions

	mu   sync.Mutex
	open map[string]int
}

func (h *roomsWeb) authenticate(r *http.Request) (string, int, string) {
	header := r.Header.Get("Authorization")
	credential, found := strings.CutPrefix(header, "Bearer ")
	if !found {
		credential, found = strings.CutPrefix(header, "bearer ")
	}
	credential = strings.TrimSpace(credential)
	if !found || credential == "" {
		return "", http.StatusUnauthorized, "a bearer credential is required"
	}
	account, err := h.core.accounts.Lookup(r.Context(), credential)
	if err != nil {
		return "", http.StatusUnauthorized, "invalid credentials"
	}
	switch requested := r.URL.Query().Get("tenant"); {
	case account.IsRelay || account.IsRecorder:
		return "", http.StatusForbidden, "relay and recorder credentials cannot read rooms"
	case account.Tenant == "":
		return "", http.StatusForbidden, "this endpoint is for a tenant's own backend key; operators use the admin API"
	case requested != "" && requested != account.Tenant:
		return "", http.StatusForbidden, "this credential is scoped to another tenant"
	case !account.Grants.IsOwner:
		return "", http.StatusForbidden, "reading rooms here needs a backend key with owner standing"
	}
	return account.Tenant, http.StatusOK, ""
}

type roomView struct {
	Room      string     `json:"room"`
	Lifecycle string     `json:"lifecycle"`
	Owner     string     `json:"owner,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	StartsAt  *time.Time `json:"starts_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	Devices   uint32     `json:"devices"`
	People    uint32     `json:"people"`
	Recording bool       `json:"recording"`
}

func optionalTime(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	return &at
}

func (h *roomsWeb) list(w http.ResponseWriter, r *http.Request) {
	tenant, code, refusal := h.authenticate(r)
	if code != http.StatusOK {
		liveError(w, code, refusal)
		return
	}
	query := store.RoomQuery{Tenant: tenant, Paging: store.Paging{PageSize: largestRoomsPage, PageToken: r.URL.Query().Get("page_token")}}
	if raw := r.URL.Query().Get("page_size"); raw != "" {
		size, err := strconv.Atoi(raw)
		if err != nil || size < 1 || size > largestRoomsPage {
			liveError(w, http.StatusBadRequest, "page_size must be 1 to 100")
			return
		}
		query.PageSize = uint32(size)
	}
	for _, name := range strings.Split(r.URL.Query().Get("lifecycle"), ",") {
		switch lifecycle := control.Lifecycle(strings.TrimSpace(name)); lifecycle {
		case "":
		case control.LifecycleScheduled, control.LifecycleLive, control.LifecycleActingHost, control.LifecycleEnded:
			query.Lifecycle = append(query.Lifecycle, lifecycle)
		default:
			liveError(w, http.StatusBadRequest, "lifecycle must be scheduled, live, acting_host or ended")
			return
		}
	}
	page, err := h.core.store.ListRooms(r.Context(), query)
	if err != nil {
		liveError(w, http.StatusBadRequest, "the rooms could not be listed")
		return
	}
	rooms := make([]roomView, 0, len(page.Rooms))
	for _, room := range page.Rooms {
		counts, err := h.core.counts(r.Context(), room.Tenant, room.Room)
		if err != nil {
			liveError(w, http.StatusInternalServerError, "internal error")
			return
		}
		rooms = append(rooms, roomView{
			Room: room.Room, Lifecycle: string(room.Lifecycle), Owner: room.Owner, CreatedAt: room.CreatedAt,
			StartsAt: optionalTime(room.StartsAt), EndedAt: optionalTime(room.EndedAt),
			Devices: counts.devices, People: counts.people, Recording: counts.recording,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"rooms": rooms, "next_page_token": page.NextPageToken})
}

func (h *roomsWeb) acquire(tenant string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.open[tenant] >= h.options.MaxStreams {
		return false
	}
	h.open[tenant]++
	return true
}

func (h *roomsWeb) release(tenant string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.open[tenant]--
	if h.open[tenant] <= 0 {
		delete(h.open, tenant)
	}
}

var eventJSON = protojson.MarshalOptions{UseProtoNames: true}

// stream follows the same join as Events.StreamRoomEvents: subscribe, then
// read history, then drop live events at or below what history sent.
func (h *roomsWeb) stream(w http.ResponseWriter, r *http.Request) {
	tenant, code, refusal := h.authenticate(r)
	if code != http.StatusOK {
		liveError(w, code, refusal)
		return
	}
	room := r.URL.Query().Get("room")
	// after=0 with a room replays it from the start, up to a page.
	var after uint64
	raw, resume := r.URL.Query().Get("after"), r.URL.Query().Has("after")
	if resume {
		parsed, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			liveError(w, http.StatusBadRequest, "after must be a sequence number")
			return
		}
		if room == "" {
			liveError(w, http.StatusBadRequest, "after needs a room, since sequences are per room")
			return
		}
		after = parsed
	}
	if !h.acquire(tenant) {
		liveError(w, http.StatusTooManyRequests, "too many event streams open for this tenant")
		return
	}
	defer h.release(tenant)

	id, subscription := h.core.bus.subscribe(tenant, room, nil)
	defer h.core.bus.unsubscribe(id)

	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(event store.Event) bool {
		var decoded adminpb.RoomEvent
		if proto.Unmarshal(event.Body, &decoded) != nil {
			return true
		}
		decoded.Sequence = event.Sequence
		body, err := eventJSON.Marshal(&decoded)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "id: %d\nevent: room_event\ndata: %s\n\n", event.Sequence, body); err != nil {
			return false
		}
		return controller.Flush() == nil
	}

	if _, err := fmt.Fprint(w, "retry: 3000\n\nevent: ready\ndata: {}\n\n"); err != nil || controller.Flush() != nil {
		return
	}
	highest := after
	if resume {
		history, err := h.core.store.ListEvents(r.Context(), store.EventQuery{
			Tenant: tenant, Room: room, AfterSequence: after, Limit: store.MaxPageSize,
		})
		if err != nil {
			return
		}
		for _, event := range history {
			if !send(event) {
				return
			}
			highest = max(highest, event.Sequence)
		}
	}

	expiry := time.NewTimer(h.options.MaxStreamAge)
	defer expiry.Stop()
	recheck := time.NewTicker(h.options.Recheck)
	defer recheck.Stop()
	heartbeat := time.NewTicker(h.options.Heartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-expiry.C:
			return
		case <-recheck.C:
			if still, code, _ := h.authenticate(r); code != http.StatusOK || still != tenant {
				_, _ = fmt.Fprint(w, "event: revoked\ndata: {}\n\n")
				_ = controller.Flush()
				return
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil || controller.Flush() != nil {
				return
			}
		case event, ok := <-subscription.events:
			if !ok {
				// Dropped for not keeping up. The console resumes per room from
				// its last sequence and sees the gap rather than skipping it.
				_, _ = fmt.Fprint(w, "event: gap\ndata: {}\n\n")
				_ = controller.Flush()
				return
			}
			if room != "" && event.Sequence <= highest {
				continue
			}
			if !send(event) {
				return
			}
		}
	}
}
