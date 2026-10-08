package billing

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// Migration preview: a tenant lends MOQOM its Agora RESTful API credentials
// for one request, and gets back its Agora projects, minutes by tier, and
// what those minutes cost at Agora's list price beside an estimate of what
// they would cost here.
//
// The credentials are a customer's key to another vendor's account. They are
// held in memory for the request, sent only to api.agora.io over TLS, and
// never stored, logged, cached or put in a response or an error.
//
// Agora Console RESTful API, docs.agora.io, retrieved October 2026:
// HTTPS only, HTTP Basic with the Customer ID and Customer Secret, at most 10
// requests per second per Agora account.
//
//	GET /dev/v1/projects  {"projects":[{"id","name","vendor_key","sign_key","status","created"}]}
//	GET /dev/v3/usage?project_id=&from_date=&to_date=&business=default
//	                      {"usages":[{"date","usage":{"durationAudioAll",...}}]}, seconds per UTC day

const agoraBase = "https://api.agora.io"

// Bounds on one preview. Agora's limit is 10 requests a second per account;
// pacing at 150ms stays under it with room for the console's own calls.
const (
	agoraRequestTimeout = 10 * time.Second
	agoraPreviewTimeout = 25 * time.Second
	agoraPace           = 150 * time.Millisecond
	largestAgoraBody    = 1 << 20
	mostAgoraProjects   = 20
	agoraPreviewDays    = 90
	// largestAgoraSeconds bounds a reported duration, so a hostile or broken
	// response cannot overflow the byte estimate.
	largestAgoraSeconds = 1_000_000_000_000
)

// AgoraCredentials is a Customer ID and Customer Secret. It formats as
// redacted, so a stray log line or %v cannot print it.
type AgoraCredentials struct {
	CustomerID     string
	CustomerSecret string
}

func (AgoraCredentials) String() string         { return "[agora credentials redacted]" }
func (c AgoraCredentials) GoString() string     { return c.String() }
func (c AgoraCredentials) LogValue() slog.Value { return slog.StringValue(c.String()) }

func (c AgoraCredentials) validate() error {
	for _, value := range []string{c.CustomerID, c.CustomerSecret} {
		if value == "" || len(value) > 256 {
			return fmt.Errorf("%w: the Customer ID and Customer Secret must each be 1 to 256 characters", ErrInvalid)
		}
		for _, r := range value {
			if r <= ' ' || r > '~' {
				return fmt.Errorf("%w: the Customer ID and Customer Secret are printable ASCII without spaces", ErrInvalid)
			}
		}
	}
	if strings.Contains(c.CustomerID, ":") {
		return fmt.Errorf("%w: a Customer ID cannot contain a colon", ErrInvalid)
	}
	return nil
}

// redact removes the credentials, and the Basic header they make, from text
// that is about to be logged.
func (c AgoraCredentials) redact(text string) string {
	basic := base64.StdEncoding.EncodeToString([]byte(c.CustomerID + ":" + c.CustomerSecret))
	for _, secret := range []string{basic, c.CustomerSecret, c.CustomerID} {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	return text
}

// AgoraError is a failed call to Agora, with a message safe to show the
// tenant: fixed text, never Agora's body or a transport error.
type AgoraError struct {
	// Status is what MOQOM answers: 400 for credentials Agora rejected, 429
	// when Agora is rate limiting, 504 for a timeout, 502 otherwise.
	Status  int
	Message string
}

func (e *AgoraError) Error() string { return e.Message }

var (
	errAgoraRejected = &AgoraError{http.StatusBadRequest,
		"Agora rejected the Customer ID or Customer Secret. Check them in Agora Console under RESTful API."}
	errAgoraForbidden = &AgoraError{http.StatusBadRequest,
		"Agora refused the request for these credentials (HTTP 403)."}
	errAgoraBusy = &AgoraError{http.StatusTooManyRequests,
		"Agora is rate limiting this account. Try again in a minute."}
	errAgoraTimeout = &AgoraError{http.StatusGatewayTimeout,
		"Agora did not answer in time. Try again."}
	errAgoraUnreachable = &AgoraError{http.StatusBadGateway,
		"Couldn't reach api.agora.io. Try again."}
	errAgoraMalformed = &AgoraError{http.StatusBadGateway,
		"Agora returned a response MOQOM could not read."}
)

type agoraClient struct {
	base string
	http *http.Client
	pace time.Duration
}

// newAgoraClient talks to api.agora.io and nothing else: the base is a
// constant, redirects are not followed, and the dialer refuses non-public
// addresses, so neither a tenant nor DNS can point it inside the network.
func newAgoraClient() *agoraClient {
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: publicAddressOnly}
	return &agoraClient{
		base: agoraBase,
		pace: agoraPace,
		http: &http.Client{
			Timeout: agoraRequestTimeout,
			Transport: &http.Transport{
				Proxy:                  nil,
				DialContext:            dialer.DialContext,
				TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
				TLSHandshakeTimeout:    5 * time.Second,
				ResponseHeaderTimeout:  agoraRequestTimeout,
				MaxResponseHeaderBytes: 64 << 10,
				ForceAttemptHTTP2:      true,
				MaxIdleConns:           4,
				IdleConnTimeout:        30 * time.Second,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

var carrierGradeNAT = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func publicAddressOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || carrierGradeNAT.Contains(ip) {
		return errors.New("refusing to connect to a non-public address")
	}
	return nil
}

func (a *agoraClient) get(ctx context.Context, log *slog.Logger, credentials AgoraCredentials, path string, query url.Values, into any) error {
	target := a.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	if !strings.HasPrefix(target, "https://") {
		return errAgoraUnreachable
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return errAgoraUnreachable
	}
	request.SetBasicAuth(credentials.CustomerID, credentials.CustomerSecret)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	response, err := a.http.Do(request)
	if err != nil {
		log.WarnContext(ctx, "agora request failed", "path", path, "error", credentials.redact(err.Error()))
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return errAgoraTimeout
		}
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			return errAgoraTimeout
		}
		return errAgoraUnreachable
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, largestAgoraBody+1))
	if err != nil {
		return errAgoraUnreachable
	}
	if len(body) > largestAgoraBody {
		return &AgoraError{http.StatusBadGateway, "Agora's response was larger than MOQOM accepts."}
	}
	switch code := response.StatusCode; {
	case code == http.StatusOK:
	case code == http.StatusUnauthorized:
		return errAgoraRejected
	case code == http.StatusForbidden:
		return errAgoraForbidden
	case code == http.StatusTooManyRequests:
		return errAgoraBusy
	default:
		log.WarnContext(ctx, "agora answered an error", "path", path, "status", code)
		return &AgoraError{http.StatusBadGateway, fmt.Sprintf("Agora answered HTTP %d.", code)}
	}
	if err := json.Unmarshal(body, into); err != nil {
		return errAgoraMalformed
	}
	return nil
}

// AgoraProject is a project as the preview reports it. The App ID and App
// Certificate Agora returns beside it are never decoded.
type AgoraProject struct {
	ID      string
	Name    string
	Enabled bool
	Created time.Time
	Seconds map[Tier]int64
	// LegacyHDPlusSeconds is the part of Seconds[TierFullHD] Agora reported
	// as HDP video.
	LegacyHDPlusSeconds int64
	// Error is why this project's usage is missing, safe to show.
	Error string
}

// AgoraPreview is what one preview found. From and To are UTC dates, both
// inclusive, as Agora takes them.
type AgoraPreview struct {
	From, To            time.Time
	Days                int64
	Projects            []AgoraProject
	ProjectsTruncated   bool
	Seconds             map[Tier]int64
	LegacyHDPlusSeconds int64
}

type agoraProjects struct {
	Projects []struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Status  int    `json:"status"`
		Created int64  `json:"created"`
	} `json:"projects"`
}

type agoraUsages struct {
	Usages []struct {
		Usage map[string]json.Number `json:"usage"`
	} `json:"usages"`
}

// Agora's usage fields by the tier MOQOM bills them at. HDP is a legacy
// bucket above HD; it is counted as Full HD, the tier above 1280×720.
var agoraUsageTiers = map[string]Tier{
	"durationAudioAll":   TierAudio,
	"durationVideoHd":    TierHD,
	"durationVideoHdp":   TierFullHD,
	"durationVideo1080P": TierFullHD,
	"durationVideo2K":    Tier2K,
	"durationVideo4K":    Tier2KPlus,
}

func addSeconds(to map[Tier]int64, tier Tier, seconds int64) {
	to[tier] = min(to[tier]+seconds, largestAgoraSeconds)
}

func agoraSeconds(raw json.Number) int64 {
	value, err := raw.Float64()
	if err != nil || math.IsNaN(value) || value <= 0 {
		return 0
	}
	return int64(min(math.Round(value), largestAgoraSeconds))
}

// PreviewAgoraMigration reads a tenant's Agora projects and the last 90 full
// UTC days of their usage. A project whose usage cannot be read is reported
// with its error; credentials Agora rejects fail the whole preview.
func (s *Service) PreviewAgoraMigration(ctx context.Context, tenant string, credentials AgoraCredentials) (AgoraPreview, error) {
	if err := credentials.validate(); err != nil {
		return AgoraPreview{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, agoraPreviewTimeout)
	defer cancel()

	today := s.now().UTC().Truncate(24 * time.Hour)
	preview := AgoraPreview{
		From:    today.AddDate(0, 0, -agoraPreviewDays),
		To:      today.AddDate(0, 0, -1),
		Days:    agoraPreviewDays,
		Seconds: map[Tier]int64{},
	}
	var listed agoraProjects
	if err := s.agora.get(ctx, s.log, credentials, "/dev/v1/projects", nil, &listed); err != nil {
		s.log.InfoContext(ctx, "agora migration preview failed", "tenant", tenant, "stage", "projects")
		return AgoraPreview{}, err
	}
	projects := listed.Projects
	if len(projects) > mostAgoraProjects {
		projects, preview.ProjectsTruncated = projects[:mostAgoraProjects], true
	}
	for _, p := range projects {
		project := AgoraProject{ID: p.ID, Name: p.Name, Enabled: p.Status == 1, Seconds: map[Tier]int64{}}
		if p.Created > 0 {
			project.Created = time.Unix(p.Created, 0).UTC()
		}
		if ctx.Err() != nil {
			project.Error = "Not read: the preview ran out of time."
			preview.Projects = append(preview.Projects, project)
			continue
		}
		select {
		case <-ctx.Done():
		case <-time.After(s.agora.pace):
		}
		var usage agoraUsages
		err := s.agora.get(ctx, s.log, credentials, "/dev/v3/usage", url.Values{
			"project_id": {p.ID},
			"from_date":  {preview.From.Format(time.DateOnly)},
			"to_date":    {preview.To.Format(time.DateOnly)},
			"business":   {"default"},
		}, &usage)
		if errors.Is(err, errAgoraRejected) {
			return AgoraPreview{}, err
		}
		if err != nil {
			project.Error = err.Error()
			preview.Projects = append(preview.Projects, project)
			continue
		}
		for _, day := range usage.Usages {
			for field, raw := range day.Usage {
				tier, known := agoraUsageTiers[field]
				if !known {
					continue
				}
				seconds := agoraSeconds(raw)
				addSeconds(project.Seconds, tier, seconds)
				addSeconds(preview.Seconds, tier, seconds)
				if field == "durationVideoHdp" {
					project.LegacyHDPlusSeconds = min(project.LegacyHDPlusSeconds+seconds, largestAgoraSeconds)
					preview.LegacyHDPlusSeconds = min(preview.LegacyHDPlusSeconds+seconds, largestAgoraSeconds)
				}
			}
		}
		preview.Projects = append(preview.Projects, project)
	}
	s.log.InfoContext(ctx, "agora migration preview", "tenant", tenant, "projects", len(preview.Projects),
		"truncated", preview.ProjectsTruncated)
	return preview, nil
}

// AgoraTierPrice is one tier of a preview, priced both ways. Agora's usage
// API does not say which minutes were hosts and which broadcast audience, so
// each is priced as if all were one or the other.
type AgoraTierPrice struct {
	Tier        Tier
	Seconds     int64
	AssumedKbps int64
	// EstimatedEgressBytes is Seconds at AssumedKbps.
	EstimatedEgressBytes int64
	AgoraList            Micros
	AgoraListBroadcast   Micros
	Estimate             Micros
	EstimateBroadcast    Micros
}

// PriceAgoraUsage prices Agora minutes at Agora's list and estimates MOQOM
// Cloud's charge from the assumed bitrate of each tier, with the rate card in
// force.
func (s *Service) PriceAgoraUsage(seconds map[Tier]int64) []AgoraTierPrice {
	agora := AgoraListPrice()
	var prices []AgoraTierPrice
	for _, tier := range tierOrder {
		total := min(seconds[tier], largestAgoraSeconds)
		if total <= 0 {
			continue
		}
		kbps := AssumedKbps[tier]
		bytes := total * kbps * 125
		rate := func(role Role) Micros {
			return s.pricing.Rate(Usage{Role: role, Tier: tier, Seconds: total, EgressBytes: bytes, Hosting: HostingCloud}).Price
		}
		prices = append(prices, AgoraTierPrice{
			Tier: tier, Seconds: total, AssumedKbps: kbps, EstimatedEgressBytes: bytes,
			AgoraList:          Per1000Minutes(agora[RoleHost][tier], total),
			AgoraListBroadcast: Per1000Minutes(agora[RoleBroadcastAudience][tier], total),
			Estimate:           rate(RoleHost),
			EstimateBroadcast:  rate(RoleBroadcastAudience),
		})
	}
	return prices
}

// HTTP.

const migrationGuide = "https://moqom.dev/guides/migrate-from-agora/"

var migrationChecklist = []map[string]string{
	{"step": "Map projects to tenants and channels to rooms",
		"detail": "Each Agora project you move becomes a MOQOM tenant, or a room-name prefix in one tenant. Agora channels become MOQOM rooms, created from your backend with CreateRoom."},
	{"step": "Replace the App ID and token server",
		"detail": "Keep authenticating users as you do now. Where your token server builds an Agora token from the App ID and App Certificate, call MintToken with a MOQOM backend API key instead, naming the user, device, room and publish rights."},
	{"step": "Swap the client SDK",
		"detail": "Replace the Agora RTC SDK's join, publish and subscribe calls with the MOQOM client SDK for each platform. Check platform status first.",
		"link":   "https://moqom.dev/platforms/status/"},
	{"step": "Run side by side, then move traffic",
		"detail": "Move one room type or one region at a time and compare this console's usage summary with your Agora bill."},
	{"step": "Delete the Agora secret you used here",
		"detail": "In Agora Console, under RESTful API, delete the Customer Secret you created for this preview."},
}

func (h *handler) agoraPreview(w http.ResponseWriter, r *http.Request, key Key) {
	if !h.agoraPreviews.permit(key.Tenant) {
		w.Header().Set("Retry-After", "600")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "at most 6 Agora previews per hour per tenant"})
		return
	}
	select {
	case h.agoraInFlight <- struct{}{}:
		defer func() { <-h.agoraInFlight }()
	default:
		w.Header().Set("Retry-After", "30")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many Agora previews in progress; try again shortly"})
		return
	}
	var body struct {
		CustomerID     string `json:"customer_id"`
		CustomerSecret string `json:"customer_secret"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		// Not the decoder's message, which can quote the body.
		writeError(w, errors.Join(ErrInvalid, errors.New(`the body must be {"customer_id": "...", "customer_secret": "..."}`)))
		return
	}
	credentials := AgoraCredentials{CustomerID: strings.TrimSpace(body.CustomerID), CustomerSecret: strings.TrimSpace(body.CustomerSecret)}
	preview, err := h.service.PreviewAgoraMigration(r.Context(), key.Tenant, credentials)
	if err != nil {
		var agoraErr *AgoraError
		if errors.As(err, &agoraErr) {
			writeJSON(w, agoraErr.Status, map[string]string{"error": agoraErr.Message})
			return
		}
		writeError(w, err)
		return
	}

	type tierView struct {
		Tier                 Tier    `json:"tier"`
		Seconds              int64   `json:"seconds"`
		Minutes              float64 `json:"minutes"`
		AssumedKbps          int64   `json:"assumed_kbps"`
		EstimatedEgressBytes int64   `json:"estimated_egress_bytes"`
		AgoraList            money   `json:"agora_list"`
		AgoraListBroadcast   money   `json:"agora_list_if_broadcast_audience"`
		Estimate             money   `json:"moqom_estimate"`
		EstimateBroadcast    money   `json:"moqom_estimate_if_broadcast_audience"`
	}
	type projectView struct {
		ID                  string         `json:"id"`
		Name                string         `json:"name"`
		Enabled             bool           `json:"enabled"`
		Created             *time.Time     `json:"created,omitempty"`
		Seconds             map[Tier]int64 `json:"seconds_by_tier"`
		LegacyHDPlusSeconds int64          `json:"legacy_hd_plus_seconds,omitempty"`
		Error               string         `json:"error,omitempty"`
	}
	projects := make([]projectView, 0, len(preview.Projects))
	for _, p := range preview.Projects {
		view := projectView{ID: p.ID, Name: p.Name, Enabled: p.Enabled, Seconds: p.Seconds,
			LegacyHDPlusSeconds: p.LegacyHDPlusSeconds, Error: p.Error}
		if !p.Created.IsZero() {
			created := p.Created
			view.Created = &created
		}
		projects = append(projects, view)
	}
	tiers := []tierView{}
	var agora, agoraBroadcast, estimate, estimateBroadcast Micros
	var seconds int64
	for _, t := range h.service.PriceAgoraUsage(preview.Seconds) {
		tiers = append(tiers, tierView{
			Tier: t.Tier, Seconds: t.Seconds, Minutes: float64(t.Seconds) / 60, AssumedKbps: t.AssumedKbps,
			EstimatedEgressBytes: t.EstimatedEgressBytes,
			AgoraList:            moneyOf(t.AgoraList), AgoraListBroadcast: moneyOf(t.AgoraListBroadcast),
			Estimate: moneyOf(t.Estimate), EstimateBroadcast: moneyOf(t.EstimateBroadcast),
		})
		agora, agoraBroadcast = agora+t.AgoraList, agoraBroadcast+t.AgoraListBroadcast
		estimate, estimateBroadcast = estimate+t.Estimate, estimateBroadcast+t.EstimateBroadcast
		seconds += t.Seconds
	}
	per30 := func(m Micros) money { return moneyOf(scale(m, 30, preview.Days)) }
	notes := []string{
		"Agora figures are Agora's public list price (Interactive Live Streaming, October 2026). If you have negotiated rates, your Agora bill is lower than shown.",
		"Agora's usage API does not separate hosts from broadcast audience, so minutes are priced as hosts and interactive audience, with the all-broadcast-audience figure beside it.",
		"MOQOM figures are estimates: bytes are estimated from minutes at the assumed bitrate per tier, then priced with the published rate card. MOQOM bills measured bytes.",
	}
	if preview.LegacyHDPlusSeconds > 0 {
		notes = append(notes, "Agora reported legacy HDP (HD+) minutes; they are counted at Full HD, the tier above 1280×720.")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"estimate":           true,
		"from_date":          preview.From.Format(time.DateOnly),
		"to_date":            preview.To.Format(time.DateOnly),
		"days":               preview.Days,
		"projects":           projects,
		"projects_truncated": preview.ProjectsTruncated,
		"tiers":              tiers,
		"total": map[string]any{
			"seconds":                              seconds,
			"minutes":                              float64(seconds) / 60,
			"agora_list":                           moneyOf(agora),
			"agora_list_if_broadcast_audience":     moneyOf(agoraBroadcast),
			"moqom_estimate":                       moneyOf(estimate),
			"moqom_estimate_if_broadcast_audience": moneyOf(estimateBroadcast),
			"savings":                              moneyOf(agora - estimate),
			"savings_basis_points":                 basisPoints(agora-estimate, agora),
			"savings_if_broadcast_audience":        moneyOf(agoraBroadcast - estimateBroadcast),
		},
		"per_30_days": map[string]any{
			"agora_list": per30(agora), "moqom_estimate": per30(estimate), "savings": per30(agora - estimate),
		},
		"notes":       notes,
		"checklist":   migrationChecklist,
		"guide":       migrationGuide,
		"credentials": "Used for this request only. Not stored, logged or returned.",
	})
}
