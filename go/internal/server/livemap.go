package server

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	relaypb "github.com/moqom-cloud/moqom/go/internal/relaypb"
)

// The live connections map: where a tenant's sessions are right now, in counts
// per coarse place, for the console.
//
// Relays resolve addresses to places and send only the places (see Census in
// relay.proto). This merges every attached relay's latest census for one tenant
// and applies the disclosure rule below before anything leaves the process.
// Nothing here is stored: the input is the in-memory registry, and the output is
// computed per request.
//
// Like the rest of the relay registry, this is per control-plane replica. A
// request answered by one replica sees the relays attached to that replica.

const (
	// LivePath answers with one snapshot as JSON.
	LivePath = "/v1/live/connections"
	// LiveStreamPath answers with a snapshot every LiveOptions.Interval as
	// server-sent events.
	LiveStreamPath = "/v1/live/connections/stream"

	// A relay sends a census at least every 30 s. One not heard from in three
	// times that is describing sessions that may have gone.
	censusStale = 90 * time.Second

	disclosureFloor = 2
)

// Disclosure is how small a bucket may be before it says too much about who is
// in it.
type Disclosure struct {
	// MinCount is the fewest sessions a place, or a relay region, must hold to
	// be shown. Zero means the default of 3; anything else below 2 is raised to
	// 2, because a bucket of one is a person.
	MinCount int
	// RoundTo rounds shown counts to the nearest multiple. 1 shows them exactly.
	RoundTo int
}

func (d Disclosure) normalised() Disclosure {
	if d.MinCount < disclosureFloor {
		d.MinCount = disclosureFloor
	}
	if d.RoundTo < 1 {
		d.RoundTo = 1
	}
	return d
}

func (d Disclosure) round(count int) int {
	if d.RoundTo <= 1 {
		return count
	}
	rounded := (count + d.RoundTo/2) / d.RoundTo * d.RoundTo
	if rounded < d.RoundTo {
		return d.RoundTo
	}
	return rounded
}

// LiveOptions configures the live map endpoints.
type LiveOptions struct {
	Disclosure Disclosure
	// Interval between streamed snapshots. Defaults to 2 s.
	Interval time.Duration
	// MaxStreams is how many streams one tenant may hold open at once.
	// Defaults to 8.
	MaxStreams int
	// MaxStreamAge closes a stream after this long. The console reconnects.
	// Defaults to 15 min.
	MaxStreamAge time.Duration
	// Recheck is how often an open stream's credential is looked up again, so
	// that revoking a key ends the streams it opened. Defaults to 1 min.
	Recheck time.Duration
}

// LiveOptionsFromEnv reads MOQOM_LIVEMAP_MIN_COUNT (default 3) and
// MOQOM_LIVEMAP_ROUND_TO (default 1).
func LiveOptionsFromEnv() LiveOptions {
	read := func(name string, fallback int) int {
		if value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name))); err == nil {
			return value
		}
		return fallback
	}
	return LiveOptions{Disclosure: Disclosure{
		MinCount: read("MOQOM_LIVEMAP_MIN_COUNT", 3),
		RoundTo:  read("MOQOM_LIVEMAP_ROUND_TO", 1),
	}}
}

// LivePlace is one shown place. City and Region are empty for a country shown
// as a whole because its cities were each too small to show.
type LivePlace struct {
	Country  string `json:"country"`
	Region   string `json:"region"`
	City     string `json:"city"`
	Lat      int    `json:"lat"`
	Lon      int    `json:"lon"`
	Sessions int    `json:"sessions"`
}

// LiveRegion is the sessions served from one relay region.
type LiveRegion struct {
	Region   string `json:"region"`
	Sessions int    `json:"sessions"`
}

// LiveSnapshot is what a tenant is shown.
type LiveSnapshot struct {
	At         time.Time    `json:"at"`
	Tenant     string       `json:"tenant"`
	Sessions   int          `json:"sessions"`
	Publishers int          `json:"publishers"`
	Viewers    int          `json:"viewers"`
	Rooms      int          `json:"rooms"`
	Regions    []LiveRegion `json:"regions"`
	Places     []LivePlace  `json:"places"`
	// Withheld is sessions in no shown place: in a place below the threshold,
	// or whose address resolved to nothing.
	Withheld int `json:"withheld"`
	// RegionsWithheld is sessions on relay regions below the threshold.
	RegionsWithheld int `json:"regions_withheld"`
	Disclosure      struct {
		MinCount int `json:"min_count"`
		RoundTo  int `json:"round_to"`
	} `json:"disclosure"`
}

type placeKey struct {
	country, region, city string
	lat, lon              int
}

func (c *relayConn) recordCensus(census *relaypb.Census, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.census = census
	c.censusAt = at
}

// liveSnapshot merges every attached relay's census for one tenant and applies
// the disclosure rule.
func (c *core) liveSnapshot(tenant string, disclosure Disclosure) LiveSnapshot {
	disclosure = disclosure.normalised()
	now := c.now()

	c.relays.mu.Lock()
	connections := make([]*relayConn, 0, len(c.relays.relays))
	for _, connection := range c.relays.relays {
		connections = append(connections, connection)
	}
	c.relays.mu.Unlock()

	snapshot := LiveSnapshot{At: now, Tenant: tenant, Regions: []LiveRegion{}, Places: []LivePlace{}}
	snapshot.Disclosure.MinCount = disclosure.MinCount
	snapshot.Disclosure.RoundTo = disclosure.RoundTo

	places := map[placeKey]int{}
	regions := map[string]int{}
	rooms := map[string]struct{}{}
	unlocated := 0

	for _, connection := range connections {
		connection.mu.Lock()
		for _, session := range connection.sessions {
			if session.GetTenant() == tenant && session.GetRoom() != "" {
				rooms[session.GetRoom()] = struct{}{}
			}
		}
		census, at := connection.census, connection.censusAt
		region := connection.region
		connection.mu.Unlock()

		if census == nil || now.Sub(at) > censusStale {
			continue
		}
		for _, tally := range census.GetTenants() {
			if tally.GetTenant() != tenant || !connection.serves(tenant) {
				continue
			}
			publishers, viewers := int(tally.GetPublishers()), int(tally.GetViewers())
			snapshot.Publishers += publishers
			snapshot.Viewers += viewers
			if region == "" {
				region = "unknown"
			}
			regions[region] += publishers + viewers
			unlocated += int(tally.GetUnlocated())
			for _, place := range tally.GetPlaces() {
				key := placeKey{
					country: place.GetCountry(), region: place.GetRegion(), city: place.GetCity(),
					lat: int(place.GetLatitude()), lon: int(place.GetLongitude()),
				}
				places[key] += int(place.GetSessions())
			}
		}
	}

	snapshot.Sessions = snapshot.Publishers + snapshot.Viewers
	snapshot.Rooms = len(rooms)
	snapshot.Places, snapshot.Withheld = disclose(places, unlocated, disclosure)

	for region, sessions := range regions {
		if sessions < disclosure.MinCount {
			snapshot.RegionsWithheld += sessions
			continue
		}
		snapshot.Regions = append(snapshot.Regions, LiveRegion{Region: region, Sessions: disclosure.round(sessions)})
	}
	sort.Slice(snapshot.Regions, func(i, j int) bool { return snapshot.Regions[i].Region < snapshot.Regions[j].Region })
	return snapshot
}

// disclose decides which places may be shown.
//
// A city with at least MinCount sessions is shown as itself. Cities below it are
// pooled per country, and a pool that reaches MinCount is shown as the country,
// at the session-weighted centre of all that country's places. The rest, and
// every unlocated session, are withheld. The relay's geo module applies the same
// rule to its admin endpoint.
func disclose(places map[placeKey]int, unlocated int, disclosure Disclosure) ([]LivePlace, int) {
	type pool struct {
		pooled, weight, lat, lon int
	}
	shown := map[placeKey]int{}
	pools := map[string]*pool{}
	for key, count := range places {
		p := pools[key.country]
		if p == nil {
			p = &pool{}
			pools[key.country] = p
		}
		p.weight += count
		p.lat += key.lat * count
		p.lon += key.lon * count
		if count >= disclosure.MinCount {
			shown[key] += count
		} else {
			p.pooled += count
		}
	}

	withheld := unlocated
	for country, p := range pools {
		if p.pooled == 0 {
			continue
		}
		if p.pooled < disclosure.MinCount {
			withheld += p.pooled
			continue
		}
		key := placeKey{
			country: country,
			lat:     int(math.Round(float64(p.lat) / float64(p.weight))),
			lon:     int(math.Round(float64(p.lon) / float64(p.weight))),
		}
		shown[key] += p.pooled
	}

	out := make([]LivePlace, 0, len(shown))
	for key, count := range shown {
		out = append(out, LivePlace{
			Country: key.country, Region: key.region, City: key.city,
			Lat: key.lat, Lon: key.lon, Sessions: disclosure.round(count),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sessions != out[j].Sessions {
			return out[i].Sessions > out[j].Sessions
		}
		if out[i].Country != out[j].Country {
			return out[i].Country < out[j].Country
		}
		return out[i].City < out[j].City
	})
	return out, withheld
}

// LiveHandler serves LivePath and LiveStreamPath.
//
// Authenticated like the admin API: a bearer credential that resolves to an
// account. A tenant's account sees its own tenant and nothing else; naming
// another in `?tenant=` is refused. A platform operator's account (no tenant)
// must name one. Relay credentials and tenant accounts without owner standing
// are refused. The credential is accepted only in the Authorization header, so
// it never lands in an access log as part of a URL.
func (s *Server) LiveHandler(options LiveOptions) http.Handler {
	if options.Disclosure.MinCount == 0 {
		options.Disclosure.MinCount = 3
	}
	if options.Interval <= 0 {
		options.Interval = 2 * time.Second
	}
	if options.MaxStreams <= 0 {
		options.MaxStreams = 8
	}
	if options.MaxStreamAge <= 0 {
		options.MaxStreamAge = 15 * time.Minute
	}
	if options.Recheck <= 0 {
		options.Recheck = time.Minute
	}
	live := &liveHandler{core: s.core, options: options, open: map[string]int{}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+LivePath, live.once)
	mux.HandleFunc("GET "+LiveStreamPath, live.stream)
	return mux
}

type liveHandler struct {
	core    *core
	options LiveOptions

	mu   sync.Mutex
	open map[string]int
}

func liveError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if code == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="moqom"`)
	}
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// authenticate decides which tenant a request may see, or why it may see none.
func (h *liveHandler) authenticate(r *http.Request) (tenant string, code int, refusal string) {
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
	if account.IsRelay || account.IsRecorder {
		return "", http.StatusForbidden, "relay and recorder credentials cannot read the live map"
	}

	requested := r.URL.Query().Get("tenant")
	if account.Tenant == "" {
		if requested == "" {
			return "", http.StatusBadRequest, "an operator account must name a tenant"
		}
		return requested, http.StatusOK, ""
	}
	if requested != "" && requested != account.Tenant {
		return "", http.StatusForbidden, "this credential is scoped to another tenant"
	}
	if !account.Grants.IsOwner {
		return "", http.StatusForbidden, "the live map needs a backend key with owner standing"
	}
	return account.Tenant, http.StatusOK, ""
}

func (h *liveHandler) tenantOf(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenant, code, refusal := h.authenticate(r)
	if code != http.StatusOK {
		liveError(w, code, refusal)
		return "", false
	}
	return tenant, true
}

func (h *liveHandler) once(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOf(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(h.core.liveSnapshot(tenant, h.options.Disclosure))
}

func (h *liveHandler) acquire(tenant string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.open[tenant] >= h.options.MaxStreams {
		return false
	}
	h.open[tenant]++
	return true
}

func (h *liveHandler) release(tenant string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.open[tenant]--
	if h.open[tenant] <= 0 {
		delete(h.open, tenant)
	}
}

func (h *liveHandler) stream(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOf(w, r)
	if !ok {
		return
	}
	if !h.acquire(tenant) {
		liveError(w, http.StatusTooManyRequests, "too many live map streams open for this tenant")
		return
	}
	defer h.release(tenant)

	controller := http.NewResponseController(w)
	// The listener's write timeout bounds ordinary requests. This one is meant
	// to stay open, and is bounded by MaxStreamAge instead.
	_ = controller.SetWriteDeadline(time.Time{})

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func() bool {
		body, err := json.Marshal(h.core.liveSnapshot(tenant, h.options.Disclosure))
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", body); err != nil {
			return false
		}
		return controller.Flush() == nil
	}

	if _, err := fmt.Fprint(w, "retry: 3000\n\n"); err != nil {
		return
	}
	if !send() {
		return
	}
	ticker := time.NewTicker(h.options.Interval)
	defer ticker.Stop()
	expiry := time.NewTimer(h.options.MaxStreamAge)
	defer expiry.Stop()
	recheck := time.NewTicker(h.options.Recheck)
	defer recheck.Stop()
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
		case <-ticker.C:
			if !send() {
				return
			}
		}
	}
}
