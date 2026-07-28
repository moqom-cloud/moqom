package moqom

import (
	"context"
	"errors"
	"time"

	"github.com/moqom-cloud/moqom-go/admin/adminpb"
)

// Every call in this file returns the ModerationEntry it produced.
//
// Two reasons, and both are about after the fact. The entry is the record an appeal is
// heard against, so a backend that wants to show a moderator what they did has it
// without a second read. And its Enforced field is the honest answer to "did that
// work": true means the relays holding the affected sessions confirmed the directive,
// false means the record exists and enforcement is still catching up.

// KickRequest removes somebody from a room.
type KickRequest struct {
	// Target is who to remove. Use User for every device, Device for one.
	Target ParticipantRef

	// Code is sent to the client so it can say why. CodeOperatorAction when a human
	// decided and no category fits.
	Code ModerationCode

	Actor Actor
}

// Kick removes a participant now, without a lasting record.
//
// The difference from Ban is rejoining: a kicked participant may come straight back,
// which is the right response to a bad moment rather than a bad actor.
func (r *RoomHandle) Kick(ctx context.Context, request KickRequest) (ModerationEntry, error) {
	if request.Target.Username == "" {
		return ModerationEntry{}, errors.New("moqom: Kick needs a target")
	}
	wire := &adminpb.KickRequest{
		Tenant: r.client.tenant,
		Room:   r.name,
		Target: refToProto(request.Target),
		Code:   codeToProto(request.Code),
		Actor:  r.client.actorFor(request.Actor),
	}
	entry, err := call(ctx, r.client, "Room.Kick", func(ctx context.Context) (*adminpb.ModerationEntry, error) {
		return r.client.moderation.Kick(ctx, wire)
	})
	return entryFromProto(entry), err
}

// BanRequest refuses future sessions and ends current ones.
type BanRequest struct {
	// Subject is what the ban is keyed on: an account, a device, or an address.
	// Build it with BanAccount, BanDevice, or BanAddress.
	Subject BanSubject

	// Duration is how long. Zero is permanent, which is what the word means -- a
	// duration is the softer choice and should be the deliberate one.
	Duration time.Duration

	Code  ModerationCode
	Actor Actor
}

// Ban refuses future sessions in this room and ends current ones.
//
// Enforced in relay state and rechecked on every subscribe and publish, so a ban issued
// while the target is briefly offline still lands when they reconnect.
func (r *RoomHandle) Ban(ctx context.Context, request BanRequest) (ModerationEntry, error) {
	return ban(ctx, r.client, r.name, BanFromRoom, request)
}

// Unban lifts a ban on this room before it expires.
func (r *RoomHandle) Unban(ctx context.Context, subject BanSubject, actor Actor) (ModerationEntry, error) {
	return unban(ctx, r.client, r.name, BanFromRoom, subject, actor)
}

// BanFromTenant refuses sessions across every room in the tenant.
//
// Needs CapBanFromTenant, which is not implied by the power to ban from a room: a room
// moderator who could ban from the platform would be a room moderator with the reach of
// an operator.
func (c *Client) BanFromTenant(ctx context.Context, request BanRequest) (ModerationEntry, error) {
	return ban(ctx, c, "", BanFromTenant, request)
}

// UnbanFromTenant lifts a tenant-wide ban.
func (c *Client) UnbanFromTenant(ctx context.Context, subject BanSubject, actor Actor) (ModerationEntry, error) {
	return unban(ctx, c, "", BanFromTenant, subject, actor)
}

func ban(ctx context.Context, client *Client, room string, scope BanScope, request BanRequest) (ModerationEntry, error) {
	subject, err := subjectToProto(request.Subject)
	if err != nil {
		return ModerationEntry{}, err
	}
	wire := &adminpb.BanRequest{
		Tenant:   client.tenant,
		Room:     room,
		Scope:    banScopeToProto(scope),
		Subject:  subject,
		Duration: durationToProto(request.Duration),
		Code:     codeToProto(request.Code),
		Actor:    client.actorFor(request.Actor),
	}
	entry, err := call(ctx, client, "Ban", func(ctx context.Context) (*adminpb.ModerationEntry, error) {
		return client.moderation.Ban(ctx, wire)
	})
	return entryFromProto(entry), err
}

func unban(ctx context.Context, client *Client, room string, scope BanScope, subject BanSubject, actor Actor) (ModerationEntry, error) {
	wireSubject, err := subjectToProto(subject)
	if err != nil {
		return ModerationEntry{}, err
	}
	wire := &adminpb.UnbanRequest{
		Tenant:  client.tenant,
		Room:    room,
		Scope:   banScopeToProto(scope),
		Subject: wireSubject,
		Actor:   client.actorFor(actor),
	}
	entry, err := call(ctx, client, "Unban", func(ctx context.Context) (*adminpb.ModerationEntry, error) {
		return client.moderation.Unban(ctx, wire)
	})
	return entryFromProto(entry), err
}

// MuteRequest stops or restores somebody's media.
type MuteRequest struct {
	Target ParticipantRef

	// Media is which kinds to act on. Empty means all of them.
	Media []MediaKind

	// Scope decides who stops receiving it. Required: a participant can be under
	// both an everyone-mute and an audience-mute at once, and acting on the wrong
	// one looks like the call did nothing.
	Scope MuteScope

	// Duration is how long, for ForceMute. Zero holds until explicitly lifted.
	Duration time.Duration

	Actor Actor
}

// ForceMute stops somebody's media without asking their client to cooperate.
//
// MuteForEveryone is relay-enforced and survives reconnection. MuteForAudience narrows
// the subscription scope instead, so the other hosts still hear them -- no second
// encode, no second track.
func (r *RoomHandle) ForceMute(ctx context.Context, request MuteRequest) (ModerationEntry, error) {
	if request.Target.Username == "" {
		return ModerationEntry{}, errors.New("moqom: ForceMute needs a target")
	}
	if request.Scope == "" {
		return ModerationEntry{}, errors.New("moqom: ForceMute needs a scope")
	}
	wire := &adminpb.ForceMuteRequest{
		Tenant:   r.client.tenant,
		Room:     r.name,
		Target:   refToProto(request.Target),
		Media:    mediaToProto(request.Media),
		Scope:    muteScopeToProto(request.Scope),
		Duration: durationToProto(request.Duration),
		Actor:    r.client.actorFor(request.Actor),
	}
	entry, err := call(ctx, r.client, "Room.ForceMute", func(ctx context.Context) (*adminpb.ModerationEntry, error) {
		return r.client.moderation.ForceMute(ctx, wire)
	})
	return entryFromProto(entry), err
}

// Unmute lifts a forced mute.
//
// Scope has to match the mute being lifted. Lifting an audience-mute does nothing to an
// everyone-mute, and the call will report having succeeded because it did.
func (r *RoomHandle) Unmute(ctx context.Context, request MuteRequest) (ModerationEntry, error) {
	if request.Target.Username == "" {
		return ModerationEntry{}, errors.New("moqom: Unmute needs a target")
	}
	if request.Scope == "" {
		return ModerationEntry{}, errors.New("moqom: Unmute needs a scope")
	}
	wire := &adminpb.UnmuteRequest{
		Tenant: r.client.tenant,
		Room:   r.name,
		Target: refToProto(request.Target),
		Media:  mediaToProto(request.Media),
		Scope:  muteScopeToProto(request.Scope),
		Actor:  r.client.actorFor(request.Actor),
	}
	entry, err := call(ctx, r.client, "Room.Unmute", func(ctx context.Context) (*adminpb.ModerationEntry, error) {
		return r.client.moderation.Unmute(ctx, wire)
	})
	return entryFromProto(entry), err
}

// GrantsRequest changes what somebody may publish and moderate.
type GrantsRequest struct {
	Target ParticipantRef

	// Grants is the complete resulting state, not a delta.
	//
	// Absolute because a delta cannot be made idempotent: "add cohost" applied
	// twice is ambiguous about intent, while "these are their grants" applied twice
	// is not.
	Grants Grants

	// Preset replaces Grants.Moderation with the preset's expansion.
	// Grants.Publish is still taken from Grants, since a preset says nothing about
	// publishing.
	Preset Preset

	Actor Actor
}

// SetGrants changes what somebody may publish and moderate.
//
// Promotion is additive and rides a token reissue. Demotion is subtractive and does not
// wait for one: a demoted cohost who kept publishing until their token expired would be
// a hole, so the relay is told directly.
//
// Refused unless the actor's own capability set contains what is being granted, and
// CapAppointModerators is never delegated by anyone but an owner.
func (r *RoomHandle) SetGrants(ctx context.Context, request GrantsRequest) (ModerationEntry, error) {
	if request.Target.Username == "" {
		return ModerationEntry{}, errors.New("moqom: SetGrants needs a target")
	}
	preset, err := presetToProto(request.Preset)
	if err != nil {
		return ModerationEntry{}, err
	}
	wire := &adminpb.SetGrantsRequest{
		Tenant: r.client.tenant,
		Room:   r.name,
		Target: refToProto(request.Target),
		Grants: grantsToProto(request.Grants),
		Preset: preset,
		Actor:  r.client.actorFor(request.Actor),
	}
	entry, err := call(ctx, r.client, "Room.SetGrants", func(ctx context.Context) (*adminpb.ModerationEntry, error) {
		return r.client.moderation.SetGrants(ctx, wire)
	})
	return entryFromProto(entry), err
}

// Spotlight puts one participant on everyone's main tile.
//
// Pass the zero ParticipantRef to clear it.
func (r *RoomHandle) Spotlight(ctx context.Context, target ParticipantRef, actor Actor) (ModerationEntry, error) {
	wire := &adminpb.SpotlightRequest{
		Tenant: r.client.tenant,
		Room:   r.name,
		Actor:  r.client.actorFor(actor),
	}
	if target.Username != "" {
		wire.Target = refToProto(target)
	}
	entry, err := call(ctx, r.client, "Room.Spotlight", func(ctx context.Context) (*adminpb.ModerationEntry, error) {
		return r.client.moderation.Spotlight(ctx, wire)
	})
	return entryFromProto(entry), err
}

// LogQuery filters the audit trail.
type LogQuery struct {
	// Since and Until bound the window. Zero for unbounded.
	Since time.Time
	Until time.Time

	// Target filters to one person or device.
	Target ParticipantRef

	// Verb filters to these actions. Empty for all of them.
	Verb []ModerationVerb

	PageSize  uint32
	PageToken string
}

// Log reads this room's audit trail.
//
// Needs CapReadModerationLog, which is deliberately not implied by the power to act:
// the people who can ban and the people who audit bans are not always the same people.
func (r *RoomHandle) Log(ctx context.Context, query LogQuery) (Page[ModerationEntry], error) {
	return readLog(ctx, r.client, r.name, query)
}

// Log reads the tenant-wide audit trail, which is what an appeals queue wants.
func (c *Client) Log(ctx context.Context, query LogQuery) (Page[ModerationEntry], error) {
	return readLog(ctx, c, "", query)
}

func readLog(ctx context.Context, client *Client, room string, query LogQuery) (Page[ModerationEntry], error) {
	wire := &adminpb.ListModerationLogRequest{
		Tenant:    client.tenant,
		Room:      room,
		Since:     timeToProto(query.Since),
		Until:     timeToProto(query.Until),
		PageSize:  query.PageSize,
		PageToken: query.PageToken,
	}
	if query.Target.Username != "" {
		wire.Target = refToProto(query.Target)
	}
	for _, verb := range query.Verb {
		wire.Verb = append(wire.Verb, verbToProto(verb))
	}

	response, err := call(ctx, client, "Log", func(ctx context.Context) (*adminpb.ListModerationLogResponse, error) {
		return client.moderation.ListModerationLog(ctx, wire)
	})
	if err != nil {
		return Page[ModerationEntry]{}, err
	}

	page := Page[ModerationEntry]{Next: response.GetNextPageToken()}
	for _, entry := range response.GetEntries() {
		page.Items = append(page.Items, entryFromProto(entry))
	}
	return page, nil
}
