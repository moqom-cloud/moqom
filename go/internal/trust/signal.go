// Copyright 2026 Elliott Wave LLC. Licensed under BUSL-1.1; see LICENSING.md.
// CONFIDENTIAL: see doc.go.

package trust

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/netip"
	"time"
)

// Source is who observed a signal, which decides how far it is believed.
//
// Everything a client says about itself is a claim an attacker controls, so the
// detector weights it below what the relay and control plane saw, and treats a
// disagreement between the two as evidence in its own right.
type Source uint8

const (
	// SourceRelay is the relay's own observation of the connection: timing,
	// subscriptions, object sizes, QUIC statistics. Hard to forge without
	// actually behaving that way on the wire.
	SourceRelay Source = iota + 1
	// SourceControl is the control plane: token minting, attestation, bans.
	SourceControl
	// SourceClient is anything the client reported about itself.
	SourceClient
)

// Kind is what happened.
type Kind string

const (
	KindJoin            Kind = "join"
	KindLeave           Kind = "leave"
	KindPresence        Kind = "presence"
	KindTokenMint       Kind = "token_mint"
	KindTokenRefresh    Kind = "token_refresh"
	KindAttestation     Kind = "attestation"
	KindTransport       Kind = "transport"
	KindSubscribe       Kind = "subscribe"
	KindUnsubscribe     Kind = "unsubscribe"
	KindChat            Kind = "chat"
	KindTip             Kind = "tip"
	KindPublishStats    Kind = "publish_stats"
	KindCanaryHit       Kind = "canary_hit"
	KindClientTelemetry Kind = "client_telemetry"
)

// Pseudonym is a keyed hash of an identifier. Stable for one key and scope, and
// useless to anyone without the key; see [Pseudonymiser].
type Pseudonym string

// NetworkClass is what kind of network an address belongs to.
type NetworkClass uint8

const (
	NetworkUnknown NetworkClass = iota
	NetworkResidential
	NetworkMobile
	// NetworkHosting is a datacenter or cloud ASN. Humans do appear here --
	// corporate VDI, some VPNs -- which is why it is a weak signal on its own.
	NetworkHosting
	NetworkVPN
)

// Network is everything kept about where a connection came from. The address
// itself is not: it is classified at ingest and dropped.
type Network struct {
	ASN   uint32
	Class NetworkClass
	// Prefix is a pseudonym of the /24 (IPv4) or /48 (IPv6), tenant-scoped, so
	// that co-located joins can be linked without an address being kept.
	Prefix Pseudonym
	// Country is ISO 3166-1 alpha-2, for travel-feasibility checks only.
	Country string
}

// Transport is a relay-side summary of one QUIC connection over a window.
type Transport struct {
	RTT       time.Duration
	RTTVar    time.Duration
	LossRatio float64
	// Fingerprint is a pseudonym of the client's QUIC transport parameters and
	// TLS ClientHello shape -- the stack, not the person. Headless clients and
	// scripted stacks have distinctive ones.
	Fingerprint Pseudonym
	// HeadlessHint is set by the ingest layer when Fingerprint is on the
	// operator's private list of known automation stacks.
	HeadlessHint bool
}

// PublishStats is what the relay can say about a published track without
// decoding it: object timing, object sizes, and group (keyframe) boundaries.
// It works under end-to-end encryption, because none of it needs the payload.
type PublishStats struct {
	Track  string
	Window time.Duration
	// Intervals are object inter-arrival gaps as seen at the relay, bounded.
	IntervalMean time.Duration
	IntervalStd  time.Duration
	// GroupFrames is frames per group: the keyframe cadence.
	GroupFramesMean float64
	GroupFramesStd  float64
	// Sizes is a bounded sample of consecutive delta-object sizes in bytes,
	// excluding the first object of each group.
	Sizes []uint32
	// Recurrence is ReplayIndex.Recurrence over the track so far.
	Recurrence float64
	// Encoder is a pseudonym of the encoder's identifying configuration, from
	// the catalog or the sequence header, where the relay can see one.
	Encoder Pseudonym
}

// ClientClaim is what a client said about itself. Kept beside the relay's view
// so they can be compared; never trusted on its own.
type ClientClaim struct {
	FrameRate float64
	Bitrate   float64
	Platform  string
}

// Subject is who an event is about, in pseudonyms.
type Subject struct {
	Tenant string
	Room   string
	// Account is the tenant's username, pseudonymised per tenant.
	Account Pseudonym
	// Device is the device key thumbprint, pseudonymised per tenant.
	Device Pseudonym
	// GlobalDevice is the same thumbprint pseudonymised with the platform key,
	// which links one device across tenants without telling any tenant which.
	// Only the ring detector reads it.
	GlobalDevice Pseudonym
}

// Key identifies one participant in one room.
func (s Subject) Key() string {
	return s.Tenant + "\x00" + s.Room + "\x00" + string(s.Account) + "\x00" + string(s.Device)
}

// Event is one privacy-minimised observation.
//
// What it does not contain is as deliberate as what it does: no media, no
// address, no username, no chat text. A tip carries an amount band, not an
// amount, because cadence is the signal and the money is MOTOM's business.
type Event struct {
	At      time.Time
	Kind    Kind
	Source  Source
	Subject Subject
	// Publisher is set when the participant holds a publish grant.
	Publisher bool

	Network   *Network
	Transport *Transport
	Publish   *PublishStats
	Claim     *ClientClaim

	// Track is the track name for subscribe and unsubscribe.
	Track string
	// Presence is the new presence for KindPresence.
	Presence string
	// TokenID is a pseudonym of the token's jti for mint and refresh.
	TokenID Pseudonym
	// Attestation fields, for KindAttestation.
	HardwareBacked bool
	AttestCounter  uint64
	ProofNonce     Pseudonym
	// AmountBand is log10 of a tip amount in micros, rounded.
	AmountBand int
}

// Pseudonymiser turns identifiers into [Pseudonym]s.
//
// Tenant-scoped pseudonyms cannot be joined across tenants, so a tenant's
// moderators can see "the same device as last week" without that device being
// linkable to anything another tenant holds. The platform scope exists for one
// consumer, cross-tenant ring detection, and its outputs never leave this
// package.
type Pseudonymiser struct {
	key []byte
}

// NewPseudonymiser takes a secret from Secret Manager. Rotating it severs every
// existing link, which is the intended way to forget.
func NewPseudonymiser(key []byte) *Pseudonymiser {
	return &Pseudonymiser{key: append([]byte(nil), key...)}
}

// Tenant pseudonymises value within one tenant.
func (p *Pseudonymiser) Tenant(tenant, kind, value string) Pseudonym {
	return p.sum("t", tenant, kind, value)
}

// Platform pseudonymises value across tenants.
func (p *Pseudonymiser) Platform(kind, value string) Pseudonym {
	return p.sum("p", "", kind, value)
}

func (p *Pseudonymiser) sum(scope, tenant, kind, value string) Pseudonym {
	mac := hmac.New(sha256.New, p.key)
	for _, part := range []string{scope, tenant, kind, value} {
		mac.Write([]byte(part))
		mac.Write([]byte{0})
	}
	return Pseudonym(base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16]))
}

// NetworkResolver classifies an address. Implemented over an ASN database the
// operator licenses; the trust package never sees the database or the address
// beyond this call.
type NetworkResolver interface {
	Resolve(addr netip.Addr) (asn uint32, class NetworkClass, country string)
}

// ClassifyAddress builds the [Network] for an address the relay saw, and is the
// only place in this package that touches one.
func ClassifyAddress(p *Pseudonymiser, resolver NetworkResolver, tenant, remote string) *Network {
	addrPort, err := netip.ParseAddrPort(remote)
	var addr netip.Addr
	if err == nil {
		addr = addrPort.Addr()
	} else if addr, err = netip.ParseAddr(remote); err != nil {
		return &Network{}
	}
	addr = addr.Unmap()
	bits := 48
	if addr.Is4() {
		bits = 24
	}
	prefix, _ := addr.Prefix(bits)
	network := &Network{Prefix: p.Tenant(tenant, "prefix", prefix.String())}
	if resolver != nil {
		network.ASN, network.Class, network.Country = resolver.Resolve(addr)
	}
	return network
}
