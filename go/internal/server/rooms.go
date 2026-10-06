package server

import (
	"context"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	adminpb "github.com/moqom-cloud/moqom-go/admin/adminpb"
	"github.com/moqom-cloud/moqom/go/internal/control"
	relaypb "github.com/moqom-cloud/moqom/go/internal/relaypb"
	"github.com/moqom-cloud/moqom/go/internal/store"
)

type roomsService struct {
	adminpb.UnimplementedRoomsServer
	core *core
}

func (s *roomsService) CreateRoom(ctx context.Context, request *adminpb.CreateRoomRequest) (*adminpb.Room, error) {
	account, err := requireTenant(ctx, request.GetTenant())
	if err != nil {
		return nil, err
	}
	if request.GetRoom() == "" {
		return nil, status.Error(codes.InvalidArgument, "room is required")
	}
	// Creating a room is not a moderation action over anybody, so it needs a
	// capability and no standing. Room policy is the one that matters: an account
	// that cannot change policy should not be able to set it either, or the check on
	// updates is decoration.
	if !account.Grants.IsOwner && !account.Grants.Moderation.Has(control.CapChangeRoomPolicy) {
		return nil, status.Error(codes.PermissionDenied, "creating a room needs change_room_policy")
	}

	policy, err := policyFromProto(request.GetPolicy())
	if err != nil {
		return nil, statusFor(err)
	}
	// Measured against the default rather than against nothing, so a room born
	// unencrypted is held to the same rule as one that is made so later. Otherwise
	// deleting and recreating a room would be the way around the audit trail.
	defaults := control.Policy{}.WithDefaults()
	if err := requireReasonToLoosen(defaults, policy, request.GetActor()); err != nil {
		return nil, err
	}

	owner := ""
	if request.GetOwnerUsername() != "" {
		owner, err = control.NormaliseUsername(request.GetOwnerUsername())
		if err != nil {
			return nil, statusFor(err)
		}
	}

	lifecycle := control.LifecycleLive
	startsAt := timeFromProto(request.GetStartsAt())
	if !startsAt.IsZero() {
		lifecycle = control.LifecycleScheduled
	}

	room := control.Room{
		Tenant:    request.GetTenant(),
		Room:      request.GetRoom(),
		Policy:    policy,
		Lifecycle: lifecycle,
		Owner:     owner,
		StartsAt:  startsAt,
		CreatedAt: s.core.now(),
	}
	if err := room.Namespace().Validate(); err != nil {
		return nil, statusFor(err)
	}

	created, err := s.core.store.CreateRoom(ctx, room)
	if err != nil {
		return nil, statusFor(err)
	}
	if change := defaults.SecurityChange(created.Policy); change != "" {
		entry, err := s.core.record(ctx, account, request.GetActor(), control.ModerationEntry{
			Tenant:       created.Tenant,
			Room:         created.Room,
			Verb:         control.VerbUpdateRoomPolicy,
			PolicyChange: change,
		}, true)
		if err != nil {
			return nil, statusFor(err)
		}
		s.core.announce(ctx, entry)
	}
	s.core.followRecordingPolicy(ctx, account, request.GetActor(), created)
	return roomToProto(created, roomCounts{}), nil
}

func (s *roomsService) GetRoom(ctx context.Context, request *adminpb.GetRoomRequest) (*adminpb.Room, error) {
	if _, err := requireTenant(ctx, request.GetTenant()); err != nil {
		return nil, err
	}
	room, err := s.core.store.GetRoom(ctx, request.GetTenant(), request.GetRoom())
	if err != nil {
		return nil, statusFor(err)
	}
	counts, err := s.core.counts(ctx, room.Tenant, room.Room)
	if err != nil {
		return nil, statusFor(err)
	}
	return roomToProto(room, counts), nil
}

func (s *roomsService) ListRooms(ctx context.Context, request *adminpb.ListRoomsRequest) (*adminpb.ListRoomsResponse, error) {
	if _, err := requireTenant(ctx, request.GetTenant()); err != nil {
		return nil, err
	}

	var lifecycles []control.Lifecycle
	for _, wanted := range request.GetLifecycle() {
		mapped, ok := lifecycleByProto[wanted]
		if !ok {
			return nil, status.Errorf(codes.InvalidArgument, "unknown lifecycle %v", wanted)
		}
		lifecycles = append(lifecycles, mapped)
	}

	page, err := s.core.store.ListRooms(ctx, store.RoomQuery{
		Tenant:    request.GetTenant(),
		Lifecycle: lifecycles,
		Paging:    store.Paging{PageSize: request.GetPageSize(), PageToken: request.GetPageToken()},
	})
	if err != nil {
		return nil, statusFor(err)
	}

	response := &adminpb.ListRoomsResponse{NextPageToken: page.NextPageToken}
	for _, room := range page.Rooms {
		// Counts per room, which is a read per room. Acceptable for a page of a
		// hundred and the reason page sizes are capped; a listing that summarised
		// nothing would send every caller straight to GetRoom anyway.
		counts, err := s.core.counts(ctx, room.Tenant, room.Room)
		if err != nil {
			return nil, statusFor(err)
		}
		response.Rooms = append(response.Rooms, roomToProto(room, counts))
	}
	return response, nil
}

func (s *roomsService) UpdateRoomPolicy(ctx context.Context, request *adminpb.UpdateRoomPolicyRequest) (*adminpb.Room, error) {
	account, err := requireTenant(ctx, request.GetTenant())
	if err != nil {
		return nil, err
	}

	actor, err := s.core.effectiveActor(ctx, account, request.GetTenant(), request.GetRoom(), request.GetActor())
	if err != nil {
		return nil, statusFor(err)
	}
	if err := control.Authorise(actor, control.VerbUpdateRoomPolicy, nil); err != nil {
		return nil, statusFor(err)
	}

	room, err := s.core.store.GetRoom(ctx, request.GetTenant(), request.GetRoom())
	if err != nil {
		return nil, statusFor(err)
	}

	next, err := policyFromProto(request.GetPolicy())
	if err != nil {
		return nil, statusFor(err)
	}
	next = applyMask(room.Policy, next, request.GetUpdateMask())
	if err := room.Policy.ValidateChange(next); err != nil {
		return nil, statusFor(err)
	}
	if err := requireReasonToLoosen(room.Policy, next, request.GetActor()); err != nil {
		return nil, err
	}
	change := room.Policy.SecurityChange(next)

	room.Policy = next
	saved, err := s.core.store.SaveRoom(ctx, room)
	if err != nil {
		return nil, statusFor(err)
	}

	entry, err := s.core.record(ctx, account, request.GetActor(), control.ModerationEntry{
		Tenant:       room.Tenant,
		Room:         room.Room,
		Verb:         control.VerbUpdateRoomPolicy,
		PolicyChange: change,
	}, true)
	if err != nil {
		return nil, statusFor(err)
	}
	// A security change is announced on the room's event stream as well as
	// logged, so a backend watching the room learns that encryption went off
	// without having to poll the audit log for it.
	if change != "" {
		s.core.announce(ctx, entry)
	}
	// A policy change is not a directive, so relays are resynchronised rather than
	// told a delta: a tightened participant limit has to reach them, and there is no
	// verb for "the rules changed".
	s.core.broadcastSnapshot(ctx, roomRef{room.Tenant, room.Room})
	s.core.followRecordingPolicy(ctx, account, request.GetActor(), saved)

	counts, err := s.core.counts(ctx, room.Tenant, room.Room)
	if err != nil {
		return nil, statusFor(err)
	}
	return roomToProto(saved, counts), nil
}

// requireReasonToLoosen refuses a change that weakens a room's security unless
// the actor says why.
//
// Turning end-to-end encryption off is allowed -- some rooms have to be
// transcribed or recorded server-side, and that is the tenant's call -- but it is
// never allowed silently. The reason goes into the audit entry beside the
// service account and the person it was on behalf of, which is what makes the
// answer to "when did this room stop being encrypted, and who decided" a lookup
// rather than an investigation.
func requireReasonToLoosen(current, next control.Policy, actor *adminpb.ActorContext) error {
	if !current.LoosensSecurity(next) {
		return nil
	}
	if actor.GetReason() == "" {
		return status.Error(codes.InvalidArgument,
			"turning off end-to-end encryption needs actor.reason; it is recorded in the moderation log")
	}
	return nil
}

// applyMask returns next where the mask names a field, and current elsewhere.
//
// An absent mask means the whole policy, which is a read-modify-write and will
// clobber a concurrent change. Documented on the request rather than prevented,
// because the alternative -- refusing to accept a policy without a mask -- makes the
// simple case awkward for the many callers who are the only writer.
func applyMask(current, next control.Policy, mask []string) control.Policy {
	if len(mask) == 0 {
		return next
	}
	result := current
	for _, field := range mask {
		switch field {
		case "age_gate":
			result.AgeGate = next.AgeGate
		case "recording":
			result.Recording = next.Recording
		case "residency_zone":
			result.ResidencyZone = next.ResidencyZone
		case "max_participants":
			result.MaxParticipants = next.MaxParticipants
		case "stage_slots":
			result.StageSlots = next.StageSlots
		case "watchdog_limit":
			result.WatchdogLimit = next.WatchdogLimit
		case "e2ee":
			result.E2EE = next.E2EE
		}
	}
	return result
}

func (s *roomsService) CloseRoom(ctx context.Context, request *adminpb.CloseRoomRequest) (*adminpb.Room, error) {
	account, err := requireTenant(ctx, request.GetTenant())
	if err != nil {
		return nil, err
	}

	actor, err := s.core.effectiveActor(ctx, account, request.GetTenant(), request.GetRoom(), request.GetActor())
	if err != nil {
		return nil, statusFor(err)
	}
	if err := control.Authorise(actor, control.VerbCloseRoom, nil); err != nil {
		return nil, statusFor(err)
	}

	room, err := s.core.store.GetRoom(ctx, request.GetTenant(), request.GetRoom())
	if err != nil {
		return nil, statusFor(err)
	}

	counts, err := s.core.counts(ctx, room.Tenant, room.Room)
	if err != nil {
		return nil, statusFor(err)
	}
	// Idempotent. Closing a closed room is what a retried shutdown looks like, and
	// answering it with an error would make a well-behaved caller log an alarm.
	if !room.IsOpen() {
		return roomToProto(room, counts), nil
	}

	room.Lifecycle = control.LifecycleEnded
	room.EndedAt = s.core.now()
	saved, err := s.core.store.SaveRoom(ctx, room)
	if err != nil {
		return nil, statusFor(err)
	}

	ref := roomRef{room.Tenant, room.Room}
	_, enforced := s.core.dispatch(ctx, ref, &relaypb.Directive{
		Action: &relaypb.Directive_CloseRoom{CloseRoom: &relaypb.CloseRoom{
			Code: request.GetCode(),
		}},
	})

	if _, err := s.core.record(ctx, account, request.GetActor(), control.ModerationEntry{
		Tenant: room.Tenant,
		Room:   room.Room,
		Verb:   control.VerbCloseRoom,
		Code:   codeByProto[request.GetCode()],
	}, enforced); err != nil {
		return nil, statusFor(err)
	}
	s.core.publish(ctx, room.Tenant, room.Room, store.EventLifecycleChanged, &adminpb.RoomEvent{
		Kind: adminpb.RoomEventKind_ROOM_EVENT_KIND_LIFECYCLE_CHANGED,
		Body: &adminpb.RoomEvent_LifecycleChanged{LifecycleChanged: &adminpb.LifecycleChanged{
			From: protoByLifecycle[control.LifecycleLive],
			To:   protoByLifecycle[control.LifecycleEnded],
		}},
	})
	s.core.stopRecordings(ctx, room.Tenant, room.Room)

	return roomToProto(saved, counts), nil
}

func (s *roomsService) ListParticipants(ctx context.Context, request *adminpb.ListParticipantsRequest) (*adminpb.ListParticipantsResponse, error) {
	if _, err := requireTenant(ctx, request.GetTenant()); err != nil {
		return nil, err
	}
	page, err := s.core.store.ListParticipants(ctx, store.ParticipantQuery{
		Tenant:           request.GetTenant(),
		Room:             request.GetRoom(),
		IncludeBackstage: request.GetIncludeBackstage(),
		IncludeDeparted:  request.GetIncludeDeparted(),
		Paging:           store.Paging{PageSize: request.GetPageSize(), PageToken: request.GetPageToken()},
	})
	if err != nil {
		return nil, statusFor(err)
	}

	response := &adminpb.ListParticipantsResponse{NextPageToken: page.NextPageToken}
	for _, participant := range page.Participants {
		response.Participants = append(response.Participants, participantToProto(participant))
	}
	return response, nil
}

func (s *roomsService) GetParticipant(ctx context.Context, request *adminpb.GetParticipantRequest) (*adminpb.Participant, error) {
	if _, err := requireTenant(ctx, request.GetTenant()); err != nil {
		return nil, err
	}
	identity := identityFromProto(request.GetIdentity())
	if identity.Username == "" || identity.Device == "" {
		return nil, status.Error(codes.InvalidArgument, "a username and a device are required; ask ListParticipants about a person")
	}
	participant, err := s.core.store.GetParticipant(ctx, request.GetTenant(), request.GetRoom(), identity)
	if err != nil {
		return nil, statusFor(err)
	}
	return participantToProto(participant), nil
}

func (s *roomsService) AttachWatchdog(ctx context.Context, request *adminpb.AttachWatchdogRequest) (*adminpb.Watchdog, error) {
	account, err := requireTenant(ctx, request.GetTenant())
	if err != nil {
		return nil, err
	}

	actor, err := s.core.effectiveActor(ctx, account, request.GetTenant(), request.GetRoom(), request.GetActor())
	if err != nil {
		return nil, statusFor(err)
	}
	if err := control.Authorise(actor, control.VerbAttachWatchdog, nil); err != nil {
		return nil, statusFor(err)
	}

	room, err := s.core.store.GetRoom(ctx, request.GetTenant(), request.GetRoom())
	if err != nil {
		return nil, statusFor(err)
	}

	kind, ok := watchdogKindByProto[request.GetKind()]
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "unknown watchdog kind %v", request.GetKind())
	}
	// A room whose policy refuses recording refuses it to the tenant's own backend
	// too. A policy that only binds strangers is not a policy.
	if kind == control.WatchdogRecording && room.Policy.Recording == control.RecordingProhibited {
		return nil, status.Error(codes.FailedPrecondition, "this room's policy prohibits recording")
	}

	operator, ok := operatorByProto[request.GetOperatedBy()]
	if !ok {
		operator = control.OperatorTenant
	}
	if operator == control.OperatorThirdParty && request.GetOperatorName() == "" {
		// The room discloses who is watching, not merely that something is, so a
		// third-party observer has to be nameable.
		return nil, status.Error(codes.InvalidArgument, "a third-party watchdog needs an operator name")
	}

	scope := room.Namespace()
	if request.GetScope() != "" {
		scope = control.Namespace{request.GetScope()}
	}

	attached, err := s.core.store.AttachWatchdog(ctx, room.Tenant, room.Room, control.Watchdog{
		Kind:         kind,
		CustomKind:   request.GetCustomKind(),
		OperatedBy:   operator,
		OperatorName: request.GetOperatorName(),
		Descriptor:   request.GetDescriptor_(),
		Scope:        scope,
		Health:       control.WatchdogAttached,
		AttachedAt:   s.core.now(),
		Billable:     request.GetIsBillable(),
		PinnedLayer:  request.GetPinnedLayer(),
	})
	if err != nil {
		return nil, statusFor(err)
	}

	if _, err := s.core.record(ctx, account, request.GetActor(), control.ModerationEntry{
		Tenant: room.Tenant,
		Room:   room.Room,
		Verb:   control.VerbAttachWatchdog,
	}, true); err != nil {
		return nil, statusFor(err)
	}
	s.core.publish(ctx, room.Tenant, room.Room, store.EventWatchdogChanged, &adminpb.RoomEvent{
		Kind: adminpb.RoomEventKind_ROOM_EVENT_KIND_WATCHDOG_CHANGED,
		Body: &adminpb.RoomEvent_WatchdogChanged{WatchdogChanged: &adminpb.WatchdogChanged{
			Watchdog: watchdogToProto(attached),
		}},
	})
	return watchdogToProto(attached), nil
}

func (s *roomsService) DetachWatchdog(ctx context.Context, request *adminpb.DetachWatchdogRequest) (*adminpb.Watchdog, error) {
	account, err := requireTenant(ctx, request.GetTenant())
	if err != nil {
		return nil, err
	}

	actor, err := s.core.effectiveActor(ctx, account, request.GetTenant(), request.GetRoom(), request.GetActor())
	if err != nil {
		return nil, statusFor(err)
	}
	if err := control.Authorise(actor, control.VerbDetachWatchdog, nil); err != nil {
		return nil, statusFor(err)
	}

	detached, err := s.core.store.DetachWatchdog(ctx, request.GetTenant(), request.GetRoom(), request.GetWatchdogId())
	if err != nil {
		return nil, statusFor(err)
	}

	if _, err := s.core.record(ctx, account, request.GetActor(), control.ModerationEntry{
		Tenant: request.GetTenant(),
		Room:   request.GetRoom(),
		Verb:   control.VerbDetachWatchdog,
	}, true); err != nil {
		return nil, statusFor(err)
	}
	s.core.publish(ctx, request.GetTenant(), request.GetRoom(), store.EventWatchdogChanged, &adminpb.RoomEvent{
		Kind: adminpb.RoomEventKind_ROOM_EVENT_KIND_WATCHDOG_CHANGED,
		Body: &adminpb.RoomEvent_WatchdogChanged{WatchdogChanged: &adminpb.WatchdogChanged{
			Watchdog: watchdogToProto(detached),
			Detached: true,
		}},
	})
	return watchdogToProto(detached), nil
}

func (s *roomsService) ListWatchdogs(ctx context.Context, request *adminpb.ListWatchdogsRequest) (*adminpb.ListWatchdogsResponse, error) {
	if _, err := requireTenant(ctx, request.GetTenant()); err != nil {
		return nil, err
	}
	watchdogs, err := s.core.store.ListWatchdogs(ctx, request.GetTenant(), request.GetRoom())
	if err != nil {
		return nil, statusFor(err)
	}
	response := &adminpb.ListWatchdogsResponse{}
	for _, watchdog := range watchdogs {
		response.Watchdogs = append(response.Watchdogs, watchdogToProto(watchdog))
	}
	return response, nil
}

// record writes an audit entry, honouring the caller's idempotency key.
//
// Shared by every mutating method, including the ones on Rooms, because "who closed
// this room" is a moderation question even when the verb is administrative.
func (c *core) record(
	ctx context.Context,
	account ServiceAccount,
	actor *adminpb.ActorContext,
	entry control.ModerationEntry,
	enforced bool,
) (control.ModerationEntry, error) {
	entry.ServiceAccount = account.Name
	entry.OnBehalfOf = actor.GetOnBehalfOf()
	entry.Reason = actor.GetReason()
	entry.IdempotencyKey = actor.GetIdempotencyKey()
	entry.Enforced = enforced
	if entry.At.IsZero() {
		entry.At = c.now()
	}
	if entry.ID == "" {
		entry.ID = c.ids()
	}

	written, err := c.store.AppendModerationEntry(ctx, entry)
	if err != nil {
		return control.ModerationEntry{}, fmt.Errorf("recording the action: %w", err)
	}
	return written, nil
}
