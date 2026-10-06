package billing

import "math/bits"

// Role is how a participant took part, which is what the ceiling is priced on.
type Role string

const (
	// RoleHost publishes.
	RoleHost Role = "host"
	// RoleAudience watches at interactive latency and may be brought on stage.
	RoleAudience Role = "audience"
	// RoleBroadcastAudience only watches.
	RoleBroadcastAudience Role = "broadcast_audience"
)

// Tier is the aggregate resolution a participant received.
type Tier string

const (
	TierAudio  Tier = "audio"
	TierHD     Tier = "hd"
	TierFullHD Tier = "full_hd"
	Tier2K     Tier = "2k"
	Tier2KPlus Tier = "2k_plus"
)

// TierFor classifies by the sum of the pixels of every video stream a
// participant received at once, the same boundaries Agora bills on, so the
// ceiling compares like with like.
func TierFor(audioOnly bool, aggregatePixels uint64) Tier {
	switch {
	case audioOnly || aggregatePixels == 0:
		return TierAudio
	case aggregatePixels <= 1280*720:
		return TierHD
	case aggregatePixels <= 1920*1080:
		return TierFullHD
	case aggregatePixels <= 2560*1440:
		return Tier2K
	default:
		return Tier2KPlus
	}
}

// Ceiling is a price per 1,000 participant-minutes, by role and tier.
type Ceiling map[Role]map[Tier]Micros

// AgoraListPrice is Agora's Interactive Live Streaming list price per 1,000
// participant-minutes.
//
// Source: docs.agora.io, Interactive Live Streaming pricing, retrieved October
// 2026 — hosts and interactive audience $0.99 / $3.99 / $8.99 / $15.99 / $35.99,
// broadcast audience $0.59 / $1.99 / $4.59 / $7.99 / $17.99. Recheck when
// Agora changes theirs; nothing here does it automatically.
func AgoraListPrice() Ceiling {
	interactive := map[Tier]Micros{
		TierAudio:  99 * Cent,
		TierHD:     399 * Cent,
		TierFullHD: 899 * Cent,
		Tier2K:     1599 * Cent,
		Tier2KPlus: 3599 * Cent,
	}
	broadcast := map[Tier]Micros{
		TierAudio:  59 * Cent,
		TierHD:     199 * Cent,
		TierFullHD: 459 * Cent,
		Tier2K:     799 * Cent,
		Tier2KPlus: 1799 * Cent,
	}
	return Ceiling{
		RoleHost:              interactive,
		RoleAudience:          interactive,
		RoleBroadcastAudience: broadcast,
	}
}

// AgoraLessOneCent is AgoraListPrice less one cent per 1,000 minutes at every
// rate.
func AgoraLessOneCent() Ceiling {
	ceiling := Ceiling{}
	for role, tiers := range AgoraListPrice() {
		// A fresh map per role: the list shares one between hosts and
		// interactive audience.
		ceiling[role] = map[Tier]Micros{}
		for tier, rate := range tiers {
			ceiling[role][tier] = rate - Cent
		}
	}
	return ceiling
}

// AssumedKbps is the bitrate a participant is assumed to receive at each tier
// when only minutes are known, as for usage reported by another provider.
// Mirrors ASSUMED_KBPS in sites/moqom-cloud/src/data/pricing.ts. An estimate
// only: the bill is metered on bytes.
var AssumedKbps = map[Tier]int64{
	TierAudio:  64,
	TierHD:     1500,
	TierFullHD: 3500,
	Tier2K:     6000,
	Tier2KPlus: 12000,
}

// Per1000Minutes is rate applied to seconds of usage.
func Per1000Minutes(rate Micros, seconds int64) Micros { return scale(rate, seconds, 60*1000) }

// Costs is what Google Cloud charges MOQOM, per unit.
//
// Configuration rather than constants, because these are the numbers that
// change when a pricing agreement is signed.
type Costs struct {
	// EgressPerGiB is internet egress from the relay.
	EgressPerGiB Micros
	// IngressPerGiB is internet ingress, free on GCP today.
	IngressPerGiB Micros
	// ConnectedSecond is compute, amortised over connected participants.
	//
	// Small: one vCPU forwards about 1.3 Gbit/s (docs/BENCHMARKS-AND-SPECS.md),
	// so compute is a rounding error beside the egress it produces. Kept as a
	// meter so a cheaper egress deal does not leave it unpriced.
	ConnectedSecond Micros
}

// GCPListPrice is Premium Tier internet egress from North America at the first
// volume band, which is the most expensive case and the safe default before a
// negotiated rate exists.
func GCPListPrice() Costs {
	return Costs{
		EgressPerGiB:    120_000, // $0.12
		IngressPerGiB:   0,
		ConnectedSecond: 1, // $0.000001, about $0.0036 per participant-hour
	}
}

// Hosting is who runs the relay that served the usage.
type Hosting string

const (
	// HostingCloud is a relay MOQOM runs, so MOQOM paid for the bytes.
	HostingCloud Hosting = "cloud"
	// HostingSelf is a relay the tenant runs, so the tenant paid for the bytes
	// and owes only the platform fee.
	HostingSelf Hosting = "self_hosted"
)

// Pricing turns usage into a charge.
type Pricing struct {
	Costs Costs
	// MarkupBasisPoints is added to cost: 1500 is 15%.
	MarkupBasisPoints int64
	// PlatformFeePerGiB is charged on egress from MOQOM's own relays, on top of
	// cost and markup.
	PlatformFeePerGiB Micros
	// SelfHostedFeePerGiB is the whole price for egress from a tenant's own
	// relays, where the tenant has already paid for the bytes. It is set
	// against Agora's cheapest effective rate, about $0.16/GiB for 720p-1080p
	// to a broadcast audience, so at 5¢ the fee alone stays 68% under it, and further under at lower
	// resolutions, where Agora charges more per GiB.
	SelfHostedFeePerGiB Micros
	// DirectFreeMinutesPerMonth is how many direct participant-minutes each
	// tenant has free in a calendar month (UTC), wherever its relays run.
	DirectFreeMinutesPerMonth int64
	// DirectFeePer1000Minutes is the price of direct participant-minutes past
	// the free allowance. Direct media never touches a relay, so this is the
	// whole price of being on a direct path: no cost, no markup, no cap. The
	// relay session kept as a fallback is billed as any other session.
	DirectFeePer1000Minutes Micros
	// Ceiling caps every charge. Nil is no cap.
	Ceiling Ceiling
}

// DefaultPricing is GCP list cost plus 15% and a one-cent-per-GiB platform fee
// on MOQOM's relays, five cents per GiB on a tenant's own, all capped at Agora
// less one cent. The markup and fee are placeholders for a
// commercial decision, not one.
func DefaultPricing() Pricing {
	return Pricing{
		Costs:               GCPListPrice(),
		MarkupBasisPoints:   1500,
		PlatformFeePerGiB:   Cent,
		SelfHostedFeePerGiB: 5 * Cent,
		// The owner's decision: 1,000 free a month, then 50 cents per 1,000.
		DirectFreeMinutesPerMonth: 1000,
		DirectFeePer1000Minutes:   50 * Cent,
		Ceiling:                   AgoraLessOneCent(),
	}
}

// DirectFee prices seconds of direct time for a tenant that had already used
// before seconds this month.
//
// The difference of two cumulative prices rather than a price of its own, so
// a month's fees sum to what its total would cost at once however it was cut
// into records, and no record is rounded twice.
func (p Pricing) DirectFee(before, seconds int64) Micros {
	if seconds <= 0 {
		return 0
	}
	free := max(p.DirectFreeMinutesPerMonth, 0) * 60
	billable := func(total int64) int64 { return max(total-free, 0) }
	return Per1000Minutes(p.DirectFeePer1000Minutes, billable(before+seconds)) -
		Per1000Minutes(p.DirectFeePer1000Minutes, billable(before))
}

// Usage is one participant's consumption over one interval.
type Usage struct {
	Role         Role
	Tier         Tier
	Seconds      int64
	EgressBytes  int64
	IngressBytes int64
	Hosting      Hosting
}

// Charge is what one Usage costs us and costs the tenant.
type Charge struct {
	Cost   Micros
	Price  Micros
	Capped bool
}

const gibibyte = 1 << 30

// Rate prices usage. Each component rounds half up to the nearest micro.
func (p Pricing) Rate(u Usage) Charge {
	var cost Micros
	fee := p.PlatformFeePerGiB
	if u.Hosting == HostingSelf {
		fee = p.SelfHostedFeePerGiB
	} else {
		cost = perGiB(u.EgressBytes, p.Costs.EgressPerGiB) +
			perGiB(u.IngressBytes, p.Costs.IngressPerGiB) +
			Micros(max(u.Seconds, 0))*p.Costs.ConnectedSecond
	}
	price := cost + scale(cost, p.MarkupBasisPoints, 10_000) +
		perGiB(u.EgressBytes, fee)

	charge := Charge{Cost: cost, Price: price}
	if rate, ok := p.Ceiling[u.Role][u.Tier]; ok && u.Seconds > 0 {
		if limit := Per1000Minutes(rate, u.Seconds); price > limit {
			charge.Price = limit
			charge.Capped = true
		}
	}
	return charge
}

func perGiB(bytes int64, rate Micros) Micros {
	if bytes <= 0 || rate <= 0 {
		return 0
	}
	return scale(rate, bytes, gibibyte)
}

// scale computes amount*numerator/denominator, rounded half up, without
// overflowing on a petabyte of egress.
func scale(amount Micros, numerator, denominator int64) Micros {
	if amount <= 0 || numerator <= 0 {
		return 0
	}
	hi, lo := bits.Mul64(uint64(amount), uint64(numerator))
	lo, carry := bits.Add64(lo, uint64(denominator/2), 0)
	hi += carry
	quotient, _ := bits.Div64(hi, lo, uint64(denominator))
	return Micros(quotient)
}
