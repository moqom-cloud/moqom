package billing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// UsageDetail is the JSON RecordUsage writes in a usage entry's Detail.
type UsageDetail struct {
	Role         Role    `json:"role"`
	Tier         Tier    `json:"tier"`
	Seconds      int64   `json:"seconds"`
	EgressBytes  int64   `json:"egress_bytes"`
	IngressBytes int64   `json:"ingress_bytes"`
	Hosting      Hosting `json:"hosting"`
	// Product is empty for a participant's session, and names anything else
	// charged as usage: ProductRecordingStorage.
	Product          string `json:"product,omitempty"`
	StorageByteHours int64  `json:"storage_byte_hours,omitempty"`
}

// UsageBucket is the usage entries of one role, tier, hosting and product,
// summed.
type UsageBucket struct {
	Role         Role
	Tier         Tier
	Hosting      Hosting
	Product      string
	Records      int64
	Seconds      int64
	EgressBytes  int64
	IngressBytes int64
	// Charged is what the ledger took, as a positive amount.
	Charged Micros
	// Capped counts the entries the ceiling limited.
	Capped int64
	// StorageByteHours is recording storage held, for ProductRecordingStorage.
	StorageByteHours int64
}

func (b UsageBucket) less(o UsageBucket) bool {
	if b.Product != o.Product {
		return b.Product < o.Product
	}
	if b.Role != o.Role {
		return b.Role < o.Role
	}
	if b.Tier != o.Tier {
		return b.Tier < o.Tier
	}
	return b.Hosting < o.Hosting
}

// UsageLine is a bucket beside what Agora's list price would have charged for
// the same seconds at the same role and tier.
type UsageLine struct {
	UsageBucket
	AgoraList Micros
	// Unrated is true for entries whose role or tier is not on Agora's rate
	// card, which then count in Charged and not in AgoraList.
	Unrated bool
}

func (l UsageLine) Savings() Micros { return l.AgoraList - l.Charged }

func (l *UsageLine) add(o UsageLine) {
	l.Records += o.Records
	l.Seconds += o.Seconds
	l.EgressBytes += o.EgressBytes
	l.IngressBytes += o.IngressBytes
	l.Charged += o.Charged
	l.Capped += o.Capped
	l.StorageByteHours += o.StorageByteHours
	l.AgoraList += o.AgoraList
	l.Unrated = l.Unrated || o.Unrated
}

// UsageReport is a tenant's metered usage over [From, To).
type UsageReport struct {
	From, To time.Time
	Lines    []UsageLine
	Total    UsageLine
	// Elapsed is the part of the range that has happened, which projections
	// extrapolate from.
	Elapsed time.Duration
}

// Longest range one summary covers, so one request scans a bounded slice of
// the ledger.
const longestUsageRange = 400 * 24 * time.Hour

// UsageReport prices a tenant's ledger usage against Agora's list price.
// Charged is the ledger's own sum, so it is what the tenant paid, not a
// re-rating.
func (s *Service) UsageReport(ctx context.Context, tenant string, from, to time.Time) (UsageReport, error) {
	if !from.Before(to) || to.Sub(from) > longestUsageRange {
		return UsageReport{}, fmt.Errorf("%w: from must be before to, at most 400 days apart", ErrInvalid)
	}
	buckets, err := s.store.UsageBreakdown(ctx, tenant, from, to)
	if err != nil {
		return UsageReport{}, err
	}
	agora := AgoraListPrice()
	report := UsageReport{From: from, To: to, Lines: make([]UsageLine, 0, len(buckets))}
	for _, bucket := range buckets {
		line := UsageLine{UsageBucket: bucket}
		if rate, ok := agora[bucket.Role][bucket.Tier]; ok {
			line.AgoraList = Per1000Minutes(rate, bucket.Seconds)
		} else if bucket.Product == "" {
			// Storage is compared at nothing rather than marked unrated: Agora's
			// rate card has no line for it, and leaving it out of the
			// comparison would overstate the savings.
			line.Unrated = true
		}
		report.Lines = append(report.Lines, line)
		report.Total.add(line)
	}
	end := to
	if now := s.now(); now.Before(end) {
		end = now
	}
	report.Elapsed = max(end.Sub(from), 0)
	return report, nil
}

func extrapolate(amount Micros, elapsed, over time.Duration) Micros {
	return scale(amount, int64(over/time.Second), int64(elapsed/time.Second))
}

// Scenario is a what-if: groups of identical participants, each receiving
// the same tiles, for the same minutes.
type Scenario struct {
	Hosting Hosting
	Minutes int64
	Groups  []ScenarioGroup
}

type ScenarioGroup struct {
	Role  Role
	Count int64
	// Tiles is how many video streams each participant receives at once.
	// Zero is audio only.
	Tiles      int64
	TileWidth  int64
	TileHeight int64
	// KbpsPerTile is the assumed bitrate of one tile. Zero assumes
	// AssumedKbps for the aggregate tier.
	KbpsPerTile int64
}

// ScenarioLine is one group, priced. Charge and AgoraList are for the whole
// group.
type ScenarioLine struct {
	ScenarioGroup
	Tier            Tier
	AggregatePixels int64
	Kbps            int64
	Seconds         int64
	// EgressBytes is per participant.
	EgressBytes int64
	Charge      Micros
	Capped      bool
	AgoraList   Micros
}

// Bounds on a scenario: a month of minutes, an audience of a million.
const (
	longestScenario = 31 * 24 * 60
	largestGroup    = 1_000_000
	mostTiles       = 64
	largestTileEdge = 7680
	fastestTile     = 50_000
	mostGroups      = 8
)

// Estimate prices a scenario with the rate card in force. Every number is an
// estimate from the assumed bitrate; the bill is metered on bytes.
func (s *Service) Estimate(scenario Scenario) ([]ScenarioLine, error) {
	if scenario.Hosting != HostingCloud && scenario.Hosting != HostingSelf {
		return nil, fmt.Errorf("%w: hosting must be cloud or self_hosted", ErrInvalid)
	}
	if scenario.Minutes < 1 || scenario.Minutes > longestScenario {
		return nil, fmt.Errorf("%w: minutes must be 1 to %d", ErrInvalid, longestScenario)
	}
	if len(scenario.Groups) == 0 || len(scenario.Groups) > mostGroups {
		return nil, fmt.Errorf("%w: 1 to %d groups", ErrInvalid, mostGroups)
	}
	agora := AgoraListPrice()
	seconds := scenario.Minutes * 60
	lines := make([]ScenarioLine, 0, len(scenario.Groups))
	for i, g := range scenario.Groups {
		switch {
		case !validRole(g.Role):
			return nil, fmt.Errorf("%w: group %d has an unknown role", ErrInvalid, i)
		case g.Count < 0 || g.Count > largestGroup:
			return nil, fmt.Errorf("%w: group %d count must be 0 to %d", ErrInvalid, i, largestGroup)
		case g.Tiles < 0 || g.Tiles > mostTiles:
			return nil, fmt.Errorf("%w: group %d tiles must be 0 to %d", ErrInvalid, i, mostTiles)
		case g.Tiles > 0 && (g.TileWidth < 1 || g.TileWidth > largestTileEdge || g.TileHeight < 1 || g.TileHeight > largestTileEdge):
			return nil, fmt.Errorf("%w: group %d tile size must be 1 to %d pixels a side", ErrInvalid, i, largestTileEdge)
		case g.KbpsPerTile < 0 || g.KbpsPerTile > fastestTile:
			return nil, fmt.Errorf("%w: group %d kbps per tile must be 0 to %d", ErrInvalid, i, fastestTile)
		}
		line := ScenarioLine{ScenarioGroup: g, Seconds: seconds}
		if g.Tiles > 0 {
			line.AggregatePixels = g.Tiles * g.TileWidth * g.TileHeight
		}
		line.Tier = TierFor(g.Tiles == 0, uint64(line.AggregatePixels))
		line.Kbps = AssumedKbps[line.Tier]
		if g.Tiles > 0 && g.KbpsPerTile > 0 {
			line.Kbps = g.Tiles * g.KbpsPerTile
		}
		line.EgressBytes = line.Kbps * 125 * seconds
		charge := s.pricing.Rate(Usage{Role: g.Role, Tier: line.Tier, Seconds: seconds,
			EgressBytes: line.EgressBytes, Hosting: scenario.Hosting})
		line.Charge = charge.Price * Micros(g.Count)
		line.Capped = charge.Capped
		line.AgoraList = Per1000Minutes(agora[g.Role][line.Tier], seconds) * Micros(g.Count)
		lines = append(lines, line)
	}
	return lines, nil
}

// HTTP.

type usageLineView struct {
	Role               Role    `json:"role,omitempty"`
	Tier               Tier    `json:"tier,omitempty"`
	Hosting            Hosting `json:"hosting,omitempty"`
	Product            string  `json:"product,omitempty"`
	Records            int64   `json:"records"`
	Seconds            int64   `json:"seconds"`
	ParticipantMinutes float64 `json:"participant_minutes"`
	EgressBytes        int64   `json:"egress_bytes"`
	IngressBytes       int64   `json:"ingress_bytes"`
	CappedRecords      int64   `json:"capped_records"`
	Charged            money   `json:"charged"`
	AgoraList          money   `json:"agora_list"`
	Savings            money   `json:"savings"`
	// SavingsBasisPoints is savings over Agora's list, in hundredths of a
	// percent; zero when Agora's list is zero.
	SavingsBasisPoints int64 `json:"savings_basis_points"`
	Unrated            bool  `json:"unrated,omitempty"`
	// StorageGiBMonths is recording storage, in the unit it is priced in.
	StorageGiBMonths float64 `json:"storage_gib_months,omitempty"`
}

func basisPoints(part, whole Micros) int64 {
	if whole <= 0 {
		return 0
	}
	if part < 0 {
		return -int64(scale(-part, 10_000, int64(whole)))
	}
	return int64(scale(part, 10_000, int64(whole)))
}

func viewUsageLine(l UsageLine) usageLineView {
	return usageLineView{
		Role: l.Role, Tier: l.Tier, Hosting: l.Hosting, Product: l.Product, Records: l.Records, Seconds: l.Seconds,
		ParticipantMinutes: float64(l.Seconds) / 60,
		EgressBytes:        l.EgressBytes, IngressBytes: l.IngressBytes, CappedRecords: l.Capped,
		Charged: moneyOf(l.Charged), AgoraList: moneyOf(l.AgoraList), Savings: moneyOf(l.Savings()),
		SavingsBasisPoints: basisPoints(l.Savings(), l.AgoraList), Unrated: l.Unrated,
		StorageGiBMonths: GiBMonths(l.StorageByteHours),
	}
}

type projectionView struct {
	Charged   money `json:"charged"`
	AgoraList money `json:"agora_list"`
	Savings   money `json:"savings"`
}

// project extrapolates the period's average rate; nil before an hour has
// elapsed, when a rate is mostly noise.
func project(total UsageLine, elapsed, over time.Duration) *projectionView {
	if elapsed < time.Hour {
		return nil
	}
	charged, agora := extrapolate(total.Charged, elapsed, over), extrapolate(total.AgoraList, elapsed, over)
	return &projectionView{Charged: moneyOf(charged), AgoraList: moneyOf(agora), Savings: moneyOf(agora - charged)}
}

// parseInstant accepts a date, read as midnight UTC, or an RFC 3339 time.
func parseInstant(raw string) (time.Time, error) {
	if at, err := time.Parse(time.DateOnly, raw); err == nil {
		return at, nil
	}
	return time.Parse(time.RFC3339, raw)
}

const thirtyDays = 30 * 24 * time.Hour

func (h *handler) usageSummary(w http.ResponseWriter, r *http.Request, key Key) {
	query := r.URL.Query()
	from, err := parseInstant(query.Get("from"))
	if err != nil {
		writeError(w, errors.Join(ErrInvalid, errors.New("from must be a date (2026-10-01) or an RFC 3339 time")))
		return
	}
	to, err := parseInstant(query.Get("to"))
	if err != nil {
		writeError(w, errors.Join(ErrInvalid, errors.New("to must be a date (2026-11-01) or an RFC 3339 time; it is exclusive")))
		return
	}
	report, err := h.service.UsageReport(r.Context(), key.Tenant, from, to)
	if err != nil {
		writeError(w, err)
		return
	}
	lines := make([]usageLineView, 0, len(report.Lines))
	byRole, byTier := map[Role]*UsageLine{}, map[Tier]*UsageLine{}
	var roles []Role
	var tiers []Tier
	storage := UsageLine{UsageBucket: UsageBucket{Product: ProductRecordingStorage}}
	for _, line := range report.Lines {
		lines = append(lines, viewUsageLine(line))
		if line.Product != "" {
			// Not a participant, so in neither breakdown; see "storage".
			storage.add(line)
			continue
		}
		if byRole[line.Role] == nil {
			byRole[line.Role] = &UsageLine{UsageBucket: UsageBucket{Role: line.Role}}
			roles = append(roles, line.Role)
		}
		byRole[line.Role].add(line)
		if byTier[line.Tier] == nil {
			byTier[line.Tier] = &UsageLine{UsageBucket: UsageBucket{Tier: line.Tier}}
			tiers = append(tiers, line.Tier)
		}
		byTier[line.Tier].add(line)
	}
	roleViews := make([]usageLineView, 0, len(roles))
	for _, role := range roles {
		roleViews = append(roleViews, viewUsageLine(*byRole[role]))
	}
	tierViews := make([]usageLineView, 0, len(tiers))
	for _, tier := range orderTiers(tiers) {
		tierViews = append(tierViews, viewUsageLine(*byTier[tier]))
	}
	body := map[string]any{
		"from":            report.From.UTC(),
		"to":              report.To.UTC(),
		"elapsed_seconds": int64(report.Elapsed / time.Second),
		"lines":           lines,
		"by_role":         roleViews,
		"by_tier":         tierViews,
		"total":           viewUsageLine(report.Total),
		// Hosted recording storage, which total includes.
		"storage": viewUsageLine(storage),
		"agora_basis": "Agora Interactive Live Streaming list price, October 2026: hosts and interactive " +
			"audience at the interactive rate, broadcast audience at the broadcast rate, same tiers, by the second.",
	}
	if p := project(report.Total, report.Elapsed, thirtyDays); p != nil {
		body["projection_30_days"] = p
	}
	if period := report.To.Sub(report.From); report.Elapsed < period {
		if p := project(report.Total, report.Elapsed, period); p != nil {
			body["projection_period"] = p
		}
	}
	writeJSON(w, http.StatusOK, body)
}

var tierOrder = []Tier{TierAudio, TierHD, TierFullHD, Tier2K, Tier2KPlus}

// orderTiers is tiers in rate-card order, anything unknown last.
func orderTiers(tiers []Tier) []Tier {
	seen := map[Tier]bool{}
	for _, tier := range tiers {
		seen[tier] = true
	}
	ordered := make([]Tier, 0, len(tiers))
	for _, tier := range tierOrder {
		if seen[tier] {
			ordered = append(ordered, tier)
			delete(seen, tier)
		}
	}
	for _, tier := range tiers {
		if seen[tier] {
			ordered = append(ordered, tier)
		}
	}
	return ordered
}

type scenarioBody struct {
	Hosting Hosting `json:"hosting"`
	Minutes int64   `json:"minutes"`
	Groups  []struct {
		Role        Role  `json:"role"`
		Count       int64 `json:"count"`
		Tiles       int64 `json:"tiles"`
		TileWidth   int64 `json:"tile_width"`
		TileHeight  int64 `json:"tile_height"`
		KbpsPerTile int64 `json:"kbps_per_tile"`
	} `json:"groups"`
}

func (h *handler) usageEstimate(w http.ResponseWriter, r *http.Request, _ Key) {
	var body scenarioBody
	if !decode(w, r, &body) {
		return
	}
	if len(body.Groups) > mostGroups {
		writeError(w, errors.Join(ErrInvalid, errors.New("at most 8 groups")))
		return
	}
	scenario := Scenario{Hosting: body.Hosting, Minutes: body.Minutes}
	for _, g := range body.Groups {
		scenario.Groups = append(scenario.Groups, ScenarioGroup{Role: g.Role, Count: g.Count, Tiles: g.Tiles,
			TileWidth: g.TileWidth, TileHeight: g.TileHeight, KbpsPerTile: g.KbpsPerTile})
	}
	lines, err := h.service.Estimate(scenario)
	if err != nil {
		writeError(w, err)
		return
	}
	type lineView struct {
		Role               Role    `json:"role"`
		Count              int64   `json:"count"`
		Tier               Tier    `json:"tier"`
		AggregatePixels    int64   `json:"aggregate_pixels"`
		Kbps               int64   `json:"kbps"`
		ParticipantMinutes int64   `json:"participant_minutes"`
		EgressGiB          float64 `json:"egress_gib"`
		Charge             money   `json:"charge"`
		Capped             bool    `json:"capped"`
		AgoraList          money   `json:"agora_list"`
	}
	views := make([]lineView, 0, len(lines))
	var charge, agora Micros
	var minutes int64
	var gib float64
	for _, l := range lines {
		lineGiB := float64(l.EgressBytes) * float64(l.Count) / gibibyte
		views = append(views, lineView{
			Role: l.Role, Count: l.Count, Tier: l.Tier, AggregatePixels: l.AggregatePixels, Kbps: l.Kbps,
			ParticipantMinutes: l.Count * scenario.Minutes, EgressGiB: lineGiB,
			Charge: moneyOf(l.Charge), Capped: l.Capped, AgoraList: moneyOf(l.AgoraList),
		})
		charge, agora, minutes, gib = charge+l.Charge, agora+l.AgoraList, minutes+l.Count*scenario.Minutes, gib+lineGiB
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"estimate": true,
		"groups":   views,
		"total": map[string]any{
			"participant_minutes":  minutes,
			"egress_gib":           gib,
			"charge":               moneyOf(charge),
			"agora_list":           moneyOf(agora),
			"savings":              moneyOf(agora - charge),
			"savings_basis_points": basisPoints(agora-charge, agora),
		},
		"note": "Estimate from the assumed bitrate, priced with the rate card in force. The bill is metered on bytes delivered.",
	})
}
