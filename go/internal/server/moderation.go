package server

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	adminpb "github.com/moqom-cloud/moqom-go/admin/adminpb"
	"github.com/moqom-cloud/moqom/go/internal/control"
	relaypb "github.com/moqom-cloud/moqom/go/internal/relaypb"
	"github.com/moqom-cloud/moqom/go/internal/store"
)

// Every verb here follows the same five steps, in this order, and the order is the
// design:
//
//  1. Authorise, against the intersection of the service account's ceiling and the
//     named human's standing in the room.
//  2. Check the idempotency key, so a redelivered action does not apply twice.
//  3. Write the durable state -- the ban, the mute, the override. Before the push,
//     because a directive that fails to arrive must still leave the decision in
//     force for the next session.
//  4. Push a directive to the relays and wait, bounded, for their answers.
//  5. Record the entry with whether enforcement was confirmed, publish an event, and
//     return the entry.
//
// Steps 3 and 4 in the other order would produce the failure that matters: a relay
// ejects somebody, the write then fails, and they reconnect unbanned.

type moderationService struct {
	adminpb.UnimplementedModerationServer
	core *core
}

// action is the shared shape of a moderation call.
type action struct {
	tenant string
	room   string
	verb   control.ModerationVerb
	actor  *adminpb.ActorContext
	// target is the person being acted on, if any. A zero Device means every device
	// they are signed in on.
	target control.Identity
	// needsStanding is false for verbs aimed at a room rather than a person.
	needsStanding bool
	// extra is a capability this particular call needs beyond what its verb
	// implies -- widening a ban from a room to the whole tenant, for instance.
	//
	// It has to be checked here rather than after, and that is not a style
	// preference: begin also answers a retried request from the log, so a check
	// placed after it can be skipped entirely by replaying somebody else's
	// idempotency key. That was a real hole, found by a test, and this field is
	// where it is closed.
	extra control.Capability
}

// begin performs steps 1 and 2, and returns an entry if the action was already done.
func (c *core) begin(ctx context.Context, request action) (ServiceAccount, *control.ModerationEntry, error) {
	account, err := requireTenant(ctx, request.tenant)
	if err != nil {
		return ServiceAccount{}, nil, err
	}
	if request.room == "" {
		return ServiceAccount{}, nil, status.Error(codes.InvalidArgument, "room is required")
	}

	actor, err := c.effectiveActor(ctx, account, request.tenant, request.room, request.actor)
	if err != nil {
		return ServiceAccount{}, nil, statusFor(err)
	}

	var target *control.Grants
	if request.needsStanding {
		target = c.targetGrants(ctx, request.tenant, request.room, request.target)
	}
	if err := control.Authorise(actor, request.verb, target); err != nil {
		return ServiceAccount{}, nil, statusFor(err)
	}
	if request.extra != 0 && !actor.IsOwner && !actor.Moderation.Has(request.extra) {
		return ServiceAccount{}, nil, status.Errorf(codes.PermissionDenied,
			"this call needs %s, actor holds %s", request.extra, actor.Moderation)
	}

	// The retry check comes after authorisation, so a caller cannot learn that an
	// action happened by replaying a key it was never entitled to use.
	if key := request.actor.GetIdempotencyKey(); key != "" {
		existing, err := c.store.FindModerationEntry(ctx, request.tenant, key)
		if err == nil {
			return account, &existing, nil
		}
	}
	return account, nil, nil
}

func (s *moderationService) Kick(ctx context.Context, request *adminpb.KickRequest) (*adminpb.ModerationEntry, error) {
	target, err := targetFromProto(request.GetTarget())
	if err != nil {
		return nil, statusFor(err)
	}

	account, done, err := s.core.begin(ctx, action{
		tenant: request.GetTenant(), room: request.GetRoom(), verb: control.VerbKick,
		actor: request.GetActor(), target: target, needsStanding: true,
	})
	if err != nil {
		return nil, err
	}
	if done != nil {
		return entryToProto(*done), nil
	}

	// A kick leaves no durable state on purpose: it removes somebody from the room
	// and lets them come back. That is the right response to a bad moment, and the
	// reason Ban exists separately for a bad actor.
	ref := roomRef{request.GetTenant(), request.GetRoom()}
	affected, enforced := s.core.dispatch(ctx, ref, &relaypb.Directive{
		Action: &relaypb.Directive_Eject{Eject: &relaypb.Eject{
			Target: request.GetTarget(),
			Code:   request.GetCode(),
		}},
	})

	entry, err := s.core.record(ctx, account, request.GetActor(), control.ModerationEntry{
		Tenant:           request.GetTenant(),
		Room:             request.GetRoom(),
		Verb:             control.VerbKick,
		Target:           target,
		TargetAllDevices: target.Device == "",
		Code:             codeByProto[request.GetCode()],
	}, enforced)
	if err != nil {
		return nil, statusFor(err)
	}
	s.core.announce(ctx, entry)

	// Reported rather than hidden. Zero affected sessions is not an error -- the
	// target may have already gone, or be on a relay attached to another replica --
	// but a moderator's tool needs to be able to say "nobody was there" instead of
	// "done".
	_ = affected
	return entryToProto(entry), nil
}

func (s *moderationService) Ban(ctx context.Context, request *adminpb.BanRequest) (*adminpb.ModerationEntry, error) {
	ban, err := banFromProto(request.GetSubject())
	if err != nil {
		return nil, statusFor(err)
	}

	scopeTenant := request.GetScope() == adminpb.BanScope_BAN_SCOPE_TENANT
	verb := control.VerbBan
	room := request.GetRoom()
	if scopeTenant {
		// A tenant-wide ban still needs a room to be authorised against, since
		// standing is held in a room. The stored ban is tenant-scoped.
		if room == "" {
			return nil, status.Error(codes.InvalidArgument, "banning tenant-wide still needs a room to authorise against")
		}
	}

	// Widening the scope needs the wider capability, or anyone who can ban from a
	// room bans from the platform by passing a different enum value.
	var wider control.Capability
	if scopeTenant {
		wider = control.CapBanFromTenant
	}

	account, done, err := s.core.begin(ctx, action{
		tenant: request.GetTenant(), room: room, verb: verb,
		actor: request.GetActor(), target: control.Identity{Username: ban.Username, Device: ban.Device},
		needsStanding: ban.Kind != control.BanByAddress,
		extra:         wider,
	})
	if err != nil {
		return nil, err
	}
	if done != nil {
		return entryToProto(*done), nil
	}

	var expires time.Time
	if duration := request.GetDuration().AsDuration(); duration > 0 {
		expires = s.core.now().Add(duration)
	}

	ban.Tenant = request.GetTenant()
	ban.Room = room
	if scopeTenant {
		ban.Room = ""
	}
	ban.Code = codeByProto[request.GetCode()]
	ban.Reason = request.GetActor().GetReason()
	ban.ExpiresAt = expires
	ban.CreatedAt = s.core.now()
	ban.CreatedBy = account.Name

	// Durable first. A ban that exists only as a directive is a ban that a
	// reconnection defeats.
	stored, err := s.core.store.PutBan(ctx, ban)
	if err != nil {
		return nil, statusFor(err)
	}

	ref := roomRef{request.GetTenant(), room}
	_, enforcedBan := s.core.dispatch(ctx, ref, &relaypb.Directive{
		Action: &relaypb.Directive_UpdateBan{UpdateBan: &relaypb.UpdateBan{
			Ban: banToRelay(stored), Active: true,
		}},
	})
	// And eject whoever it names, since a ban that waits for the next connection is
	// not what anybody means by banning someone who is in the room.
	var enforcedEject = true
	if ban.Kind != control.BanByAddress {
		_, enforcedEject = s.core.dispatch(ctx, ref, &relaypb.Directive{
			Action: &relaypb.Directive_Eject{Eject: &relaypb.Eject{
				Target: &adminpb.ParticipantRef{Username: ban.Username, Device: ban.Device},
				Code:   request.GetCode(),
			}},
		})
	}

	entry, err := s.core.record(ctx, account, request.GetActor(), control.ModerationEntry{
		Tenant:           request.GetTenant(),
		Room:             room,
		Verb:             control.VerbBan,
		Target:           control.Identity{Username: ban.Username, Device: ban.Device},
		TargetAllDevices: ban.Kind == control.BanByUsername,
		BanKind:          ban.Kind,
		BanSubject:       banSubjectOf(ban),
		BanScopeTenant:   scopeTenant,
		Code:             ban.Code,
		ExpiresAt:        expires,
	}, enforcedBan && enforcedEject)
	if err != nil {
		return nil, statusFor(err)
	}
	s.core.announce(ctx, entry)
	return entryToProto(entry), nil
}

func banSubjectOf(ban control.Ban) string {
	switch ban.Kind {
	case control.BanByUsername:
		return ban.Username
	case control.BanByDevice:
		return ban.Username + "/" + ban.Device
	case control.BanByAddress:
		return ban.IPPrefix
	default:
		return ""
	}
}

func (s *moderationService) Unban(ctx context.Context, request *adminpb.UnbanRequest) (*adminpb.ModerationEntry, error) {
	ban, err := banFromProto(request.GetSubject())
	if err != nil {
		return nil, statusFor(err)
	}

	scopeTenant := request.GetScope() == adminpb.BanScope_BAN_SCOPE_TENANT
	room := request.GetRoom()
	if room == "" {
		return nil, status.Error(codes.InvalidArgument, "room is required")
	}

	var wider control.Capability
	if scopeTenant {
		// Symmetric with Ban on purpose. A room moderator who could lift a
		// tenant-wide ban could readmit somebody the platform excluded, which is
		// the same power as granting it.
		wider = control.CapBanFromTenant
	}

	account, done, err := s.core.begin(ctx, action{
		tenant: request.GetTenant(), room: room, verb: control.VerbUnban,
		actor: request.GetActor(),
		// No standing check: the target is not in the room, and requiring standing
		// over somebody who has been banned would make bans unliftable once the
		// person's record has gone.
		needsStanding: false,
		extra:         wider,
	})
	if err != nil {
		return nil, err
	}
	if done != nil {
		return entryToProto(*done), nil
	}

	stored := room
	if scopeTenant {
		stored = ""
	}
	if err := s.core.store.DeleteBan(ctx, request.GetTenant(), stored, ban.Kind, ban); err != nil {
		return nil, statusFor(err)
	}

	ref := roomRef{request.GetTenant(), room}
	_, enforced := s.core.dispatch(ctx, ref, &relaypb.Directive{
		Action: &relaypb.Directive_UpdateBan{UpdateBan: &relaypb.UpdateBan{
			Ban: banToRelay(ban), Active: false,
		}},
	})

	entry, err := s.core.record(ctx, account, request.GetActor(), control.ModerationEntry{
		Tenant:         request.GetTenant(),
		Room:           room,
		Verb:           control.VerbUnban,
		Target:         control.Identity{Username: ban.Username, Device: ban.Device},
		BanKind:        ban.Kind,
		BanSubject:     banSubjectOf(ban),
		BanScopeTenant: scopeTenant,
	}, enforced)
	if err != nil {
		return nil, statusFor(err)
	}
	s.core.announce(ctx, entry)
	return entryToProto(entry), nil
}

func (s *moderationService) ForceMute(ctx context.Context, request *adminpb.ForceMuteRequest) (*adminpb.ModerationEntry, error) {
	target, err := targetFromProto(request.GetTarget())
	if err != nil {
		return nil, statusFor(err)
	}
	media, err := mediaFromProto(request.GetMedia())
	if err != nil {
		return nil, statusFor(err)
	}

	account, done, err := s.core.begin(ctx, action{
		tenant: request.GetTenant(), room: request.GetRoom(), verb: control.VerbForceMute,
		actor: request.GetActor(), target: target, needsStanding: true,
	})
	if err != nil {
		return nil, err
	}
	if done != nil {
		return entryToProto(*done), nil
	}

	audienceOnly := request.GetScope() == adminpb.MuteScope_MUTE_SCOPE_AUDIENCE
	var expires time.Time
	if duration := request.GetDuration().AsDuration(); duration > 0 {
		expires = s.core.now().Add(duration)
	}

	// Stored before the push, so a participant who reconnects comes back muted. A
	// mute a reconnection clears is not a mute.
	if _, err := s.core.store.PutMute(ctx, store.Mute{
		Tenant:       request.GetTenant(),
		Room:         request.GetRoom(),
		Identity:     target,
		Media:        media,
		AudienceOnly: audienceOnly,
		ExpiresAt:    expires,
		CreatedAt:    s.core.now(),
	}); err != nil {
		return nil, statusFor(err)
	}

	ref := roomRef{request.GetTenant(), request.GetRoom()}
	_, enforced := s.core.dispatch(ctx, ref, &relaypb.Directive{
		Action: &relaypb.Directive_ApplyMute{ApplyMute: &relaypb.ApplyMute{
			Target:       request.GetTarget(),
			Media:        request.GetMedia(),
			AudienceOnly: audienceOnly,
			Muted:        true,
			ExpiresAt:    timeToProto(expires),
		}},
	})

	s.core.updateMuteState(ctx, request.GetTenant(), request.GetRoom(), target, media, audienceOnly, true)

	entry, err := s.core.record(ctx, account, request.GetActor(), control.ModerationEntry{
		Tenant:           request.GetTenant(),
		Room:             request.GetRoom(),
		Verb:             control.VerbForceMute,
		Target:           target,
		TargetAllDevices: target.Device == "",
		Media:            media,
		AudienceOnly:     audienceOnly,
		ExpiresAt:        expires,
	}, enforced)
	if err != nil {
		return nil, statusFor(err)
	}
	s.core.announce(ctx, entry)
	return entryToProto(entry), nil
}

func (s *moderationService) Unmute(ctx context.Context, request *adminpb.UnmuteRequest) (*adminpb.ModerationEntry, error) {
	target, err := targetFromProto(request.GetTarget())
	if err != nil {
		return nil, statusFor(err)
	}
	media, err := mediaFromProto(request.GetMedia())
	if err != nil {
		return nil, statusFor(err)
	}

	account, done, err := s.core.begin(ctx, action{
		tenant: request.GetTenant(), room: request.GetRoom(), verb: control.VerbUnmute,
		actor: request.GetActor(), target: target, needsStanding: true,
	})
	if err != nil {
		return nil, err
	}
	if done != nil {
		return entryToProto(*done), nil
	}

	audienceOnly := request.GetScope() == adminpb.MuteScope_MUTE_SCOPE_AUDIENCE
	// Only the kinds named, so lifting a camera leaves a microphone muted, and
	// absent rather than an error when there was no such mute: a moderator
	// lifting a mute that has already expired has got what they wanted.
	_ = s.core.store.DeleteMute(
		ctx, request.GetTenant(), request.GetRoom(), target, media, audienceOnly)

	ref := roomRef{request.GetTenant(), request.GetRoom()}
	_, enforced := s.core.dispatch(ctx, ref, &relaypb.Directive{
		Action: &relaypb.Directive_ApplyMute{ApplyMute: &relaypb.ApplyMute{
			Target:       request.GetTarget(),
			Media:        request.GetMedia(),
			AudienceOnly: audienceOnly,
			Muted:        false,
		}},
	})

	s.core.updateMuteState(ctx, request.GetTenant(), request.GetRoom(), target, media, audienceOnly, false)

	entry, err := s.core.record(ctx, account, request.GetActor(), control.ModerationEntry{
		Tenant:       request.GetTenant(),
		Room:         request.GetRoom(),
		Verb:         control.VerbUnmute,
		Target:       target,
		Media:        media,
		AudienceOnly: audienceOnly,
	}, enforced)
	if err != nil {
		return nil, statusFor(err)
	}
	s.core.announce(ctx, entry)
	return entryToProto(entry), nil
}

func (s *moderationService) SetGrants(ctx context.Context, request *adminpb.SetGrantsRequest) (*adminpb.ModerationEntry, error) {
	target, err := targetFromProto(request.GetTarget())
	if err != nil {
		return nil, statusFor(err)
	}
	requested, err := grantsFromProto(request.GetGrants(), request.GetPreset())
	if err != nil {
		return nil, statusFor(err)
	}

	account, done, err := s.core.begin(ctx, action{
		tenant: request.GetTenant(), room: request.GetRoom(), verb: control.VerbSetGrants,
		actor: request.GetActor(), target: target, needsStanding: true,
	})
	if err != nil {
		return nil, err
	}
	if done != nil {
		return entryToProto(*done), nil
	}

	actor, err := s.core.effectiveActor(ctx, account, request.GetTenant(), request.GetRoom(), request.GetActor())
	if err != nil {
		return nil, statusFor(err)
	}
	// Nobody hands out what they do not hold, and nobody but an owner delegates the
	// power to appoint moderators. Checked here in addition to Authorise, because
	// Authorise asks whether they may act on this person and this asks whether they
	// may confer this.
	if !actor.CanGrant(requested.Moderation) {
		return nil, status.Errorf(codes.PermissionDenied,
			"holder of %s cannot grant %s", actor.Moderation, requested.Moderation)
	}
	if requested.IsOwner && !actor.IsOwner {
		return nil, status.Error(codes.PermissionDenied, "ownership is not grantable")
	}

	// The override is what makes a demotion immediate. A token already in the wild
	// may claim more; the relay is told to believe this instead.
	if err := s.core.store.PutGrantOverride(ctx, request.GetTenant(), request.GetRoom(), target, requested); err != nil {
		return nil, statusFor(err)
	}
	// Every override that names somebody applies at once, so a person-wide
	// change has to replace the device-specific ones too: otherwise promoting
	// somebody demoted on their phone would leave the phone demoted, which the
	// relay does the same way when the directive reaches it.
	if target.Device == "" {
		overrides, err := s.core.store.ListGrantOverrides(ctx, request.GetTenant(), request.GetRoom())
		if err != nil {
			return nil, statusFor(err)
		}
		for id := range overrides {
			if id.Username != target.Username || id.Device == "" {
				continue
			}
			if err := s.core.store.PutGrantOverride(ctx, request.GetTenant(), request.GetRoom(), id, requested); err != nil {
				return nil, statusFor(err)
			}
		}
	}

	previous := s.core.targetGrants(ctx, request.GetTenant(), request.GetRoom(), target)

	ref := roomRef{request.GetTenant(), request.GetRoom()}
	_, enforced := s.core.dispatch(ctx, ref, &relaypb.Directive{
		Action: &relaypb.Directive_ApplyGrants{ApplyGrants: &relaypb.ApplyGrants{
			Target: request.GetTarget(),
			Grants: grantsToProto(requested),
		}},
	})

	// The participant record follows, so a roster read agrees with the override.
	if participant, err := s.core.store.GetParticipant(ctx, request.GetTenant(), request.GetRoom(), target); err == nil {
		participant.Grants = requested
		_ = s.core.store.SaveParticipant(ctx, request.GetTenant(), request.GetRoom(), participant)
	}

	stored := requested
	entry, err := s.core.record(ctx, account, request.GetActor(), control.ModerationEntry{
		Tenant:           request.GetTenant(),
		Room:             request.GetRoom(),
		Verb:             control.VerbSetGrants,
		Target:           target,
		TargetAllDevices: target.Device == "",
		Grants:           &stored,
	}, enforced)
	if err != nil {
		return nil, statusFor(err)
	}

	s.core.publish(ctx, request.GetTenant(), request.GetRoom(), store.EventGrantsChanged, &adminpb.RoomEvent{
		Kind: adminpb.RoomEventKind_ROOM_EVENT_KIND_GRANTS_CHANGED,
		Body: &adminpb.RoomEvent_GrantsChanged{GrantsChanged: &adminpb.GrantsChanged{
			Identity: identityToProto(target),
			From:     grantsToProto(*previous),
			To:       grantsToProto(requested),
		}},
	})
	s.core.announce(ctx, entry)
	return entryToProto(entry), nil
}

func (s *moderationService) Spotlight(ctx context.Context, request *adminpb.SpotlightRequest) (*adminpb.ModerationEntry, error) {
	// A clear is a spotlight with no target, so the target is optional here and the
	// standing check only applies when there is somebody to have standing over.
	var target control.Identity
	if request.GetTarget() != nil {
		var err error
		target, err = targetFromProto(request.GetTarget())
		if err != nil {
			return nil, statusFor(err)
		}
	}

	account, done, err := s.core.begin(ctx, action{
		tenant: request.GetTenant(), room: request.GetRoom(), verb: control.VerbSpotlight,
		actor: request.GetActor(), target: target, needsStanding: target.Username != "",
	})
	if err != nil {
		return nil, err
	}
	if done != nil {
		return entryToProto(*done), nil
	}

	entry, err := s.core.record(ctx, account, request.GetActor(), control.ModerationEntry{
		Tenant: request.GetTenant(),
		Room:   request.GetRoom(),
		Verb:   control.VerbSpotlight,
		Target: target,
	}, true)
	if err != nil {
		return nil, statusFor(err)
	}

	s.core.publish(ctx, request.GetTenant(), request.GetRoom(), store.EventSpotlightChanged, &adminpb.RoomEvent{
		Kind: adminpb.RoomEventKind_ROOM_EVENT_KIND_SPOTLIGHT_CHANGED,
		Body: &adminpb.RoomEvent_SpotlightChanged{SpotlightChanged: &adminpb.SpotlightChanged{
			Identity: identityToProto(target),
		}},
	})
	s.core.announce(ctx, entry)
	return entryToProto(entry), nil
}

func (s *moderationService) ListModerationLog(
	ctx context.Context,
	request *adminpb.ListModerationLogRequest,
) (*adminpb.ListModerationLogResponse, error) {
	account, err := requireTenant(ctx, request.GetTenant())
	if err != nil {
		return nil, err
	}
	// Reading the log is its own capability, deliberately not implied by the power
	// to act: the people who can ban and the people who audit bans are not always
	// the same people, and an appeals process needs the second without the first.
	if !account.Grants.IsOwner && !account.Grants.Moderation.Has(control.CapReadModerationLog) {
		return nil, status.Error(codes.PermissionDenied, "reading the moderation log needs read_moderation_log")
	}

	var verbs []control.ModerationVerb
	for _, wanted := range request.GetVerb() {
		mapped, ok := verbByProto[wanted]
		if !ok {
			return nil, status.Errorf(codes.InvalidArgument, "unknown verb %v", wanted)
		}
		verbs = append(verbs, mapped)
	}

	var target control.Identity
	if request.GetTarget() != nil {
		target = control.Identity{
			Username: request.GetTarget().GetUsername(),
			Device:   request.GetTarget().GetDevice(),
		}
	}

	page, err := s.core.store.ListModerationLog(ctx, store.LogQuery{
		Tenant: request.GetTenant(),
		Room:   request.GetRoom(),
		Since:  timeFromProto(request.GetSince()),
		Until:  timeFromProto(request.GetUntil()),
		Target: target,
		Verbs:  verbs,
		Paging: store.Paging{PageSize: request.GetPageSize(), PageToken: request.GetPageToken()},
	})
	if err != nil {
		return nil, statusFor(err)
	}

	response := &adminpb.ListModerationLogResponse{NextPageToken: page.NextPageToken}
	for _, entry := range page.Entries {
		response.Entries = append(response.Entries, entryToProto(entry))
	}
	return response, nil
}

// announce publishes a moderation entry as a room event.
//
// So a backend that did not issue the action still learns of it -- including actions
// taken by a safety classifier, by the platform, or from another operator's console.
func (c *core) announce(ctx context.Context, entry control.ModerationEntry) {
	c.publish(ctx, entry.Tenant, entry.Room, store.EventModerationApplied, &adminpb.RoomEvent{
		Kind: adminpb.RoomEventKind_ROOM_EVENT_KIND_MODERATION_APPLIED,
		Body: &adminpb.RoomEvent_ModerationApplied{ModerationApplied: entryToProto(entry)},
	})
}

// updateMuteState keeps the participant records in step with the mute table, so a
// roster read shows what a relay is enforcing.
func (c *core) updateMuteState(
	ctx context.Context,
	tenant, room string,
	target control.Identity,
	media control.MediaKind,
	audienceOnly, muted bool,
) {
	// A target with no device names every device that person is on, so the update
	// has to be a scan rather than a lookup.
	page, err := c.store.ListParticipants(ctx, store.ParticipantQuery{
		Tenant: tenant, Room: room, IncludeBackstage: true, IncludeDeparted: true,
		Paging: store.Paging{PageSize: store.MaxPageSize},
	})
	if err != nil {
		return
	}

	for _, participant := range page.Participants {
		if participant.Identity.Username != target.Username {
			continue
		}
		if target.Device != "" && participant.Identity.Device != target.Device {
			continue
		}

		field := &participant.Mute.ForcedForEveryone
		if audienceOnly {
			field = &participant.Mute.ForcedForAudience
		}
		if muted {
			*field |= media
		} else {
			*field &^= media
		}
		if err := c.store.SaveParticipant(ctx, tenant, room, participant); err != nil {
			continue
		}
		c.publish(ctx, tenant, room, store.EventMuteChanged, &adminpb.RoomEvent{
			Kind: adminpb.RoomEventKind_ROOM_EVENT_KIND_MUTE_CHANGED,
			Body: &adminpb.RoomEvent_MuteChanged{MuteChanged: &adminpb.MuteChanged{
				Identity:               identityToProto(participant.Identity),
				SelfMuted:              mediaToProto(participant.Mute.SelfMuted),
				ForcedMutedForEveryone: mediaToProto(participant.Mute.ForcedForEveryone),
				ForcedMutedForAudience: mediaToProto(participant.Mute.ForcedForAudience),
			}},
		})
	}
}
