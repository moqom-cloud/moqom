package moqom

import (
	"context"
	"errors"
	"time"

	"github.com/moqom-cloud/moqom-go/admin/adminpb"
)

// NewRoom is the request to create a room.
type NewRoom struct {
	// Name is the room's identifier within the tenant, supplied rather than
	// generated: a customer's rooms already have names in their own system, and a
	// second identifier would mean a mapping table.
	Name string

	Policy Policy

	// Owner is the initial owner. Optional -- a room can exist before anyone joins,
	// with ownership assigned when the host's token is minted.
	Owner string

	// StartsAt creates the room scheduled rather than live.
	StartsAt time.Time

	Actor Actor
}

// CreateRoom creates a room.
//
// Idempotent on the room's name: creating one that exists returns ErrExists rather than
// replacing it, so a retry cannot silently reset a live room's policy.
func (c *Client) CreateRoom(ctx context.Context, request NewRoom) (Room, error) {
	if request.Name == "" {
		return Room{}, errors.New("moqom: CreateRoom needs a name")
	}
	wire := &adminpb.CreateRoomRequest{
		Tenant:        c.tenant,
		Room:          request.Name,
		Policy:        policyToProto(request.Policy),
		OwnerUsername: request.Owner,
		StartsAt:      timeToProto(request.StartsAt),
		Actor:         c.actorFor(request.Actor),
	}
	room, err := call(ctx, c, "CreateRoom", func(ctx context.Context) (*adminpb.Room, error) {
		return c.rooms.CreateRoom(ctx, wire)
	})
	return roomFromProto(room), err
}

// RoomQuery filters a room listing.
type RoomQuery struct {
	// Lifecycle filters to these phases. Empty for all of them.
	Lifecycle []Lifecycle

	PageSize  uint32
	PageToken string
}

// Rooms lists rooms in the tenant.
func (c *Client) Rooms(ctx context.Context, query RoomQuery) (Page[Room], error) {
	wire := &adminpb.ListRoomsRequest{
		Tenant:    c.tenant,
		PageSize:  query.PageSize,
		PageToken: query.PageToken,
	}
	for _, lifecycle := range query.Lifecycle {
		wire.Lifecycle = append(wire.Lifecycle, lifecycleToProto(lifecycle))
	}

	response, err := call(ctx, c, "Rooms", func(ctx context.Context) (*adminpb.ListRoomsResponse, error) {
		return c.rooms.ListRooms(ctx, wire)
	})
	if err != nil {
		return Page[Room]{}, err
	}

	page := Page[Room]{Next: response.GetNextPageToken()}
	for _, room := range response.GetRooms() {
		page.Items = append(page.Items, roomFromProto(room))
	}
	return page, nil
}

// AllRooms iterates every room, following page tokens.
//
// Stops on the first error, which is returned. For a large tenant, prefer Rooms with a
// page size and handle the pages yourself: this holds one page at a time but makes no
// promise about how many calls it makes.
func (c *Client) AllRooms(ctx context.Context, query RoomQuery) ([]Room, error) {
	var all []Room
	for {
		page, err := c.Rooms(ctx, query)
		if err != nil {
			return all, err
		}
		all = append(all, page.Items...)
		if page.Next == "" {
			return all, nil
		}
		query.PageToken = page.Next
	}
}

// Get reads the room.
func (r *RoomHandle) Get(ctx context.Context) (Room, error) {
	room, err := call(ctx, r.client, "Room.Get", func(ctx context.Context) (*adminpb.Room, error) {
		return r.client.rooms.GetRoom(ctx, &adminpb.GetRoomRequest{
			Tenant: r.client.tenant,
			Room:   r.name,
		})
	})
	return roomFromProto(room), err
}

// PolicyUpdate changes policy on a live room.
type PolicyUpdate struct {
	Policy Policy

	// Fields names which parts of Policy to apply, using the proto field names:
	// "age_gate", "recording", "max_participants", "stage_slots",
	// "watchdog_limit".
	//
	// Leaving it empty applies all of them, which is a read-modify-write and will
	// clobber a concurrent change. Naming the fields you meant to change is almost
	// always what you want.
	Fields []string

	Actor Actor
}

// UpdatePolicy changes policy on a live room.
//
// Tightening applies immediately, including to participants already publishing.
// Loosening does not retroactively permit what was recorded under the stricter policy.
// Residency cannot be changed at all and attempting it returns ErrInvalid.
func (r *RoomHandle) UpdatePolicy(ctx context.Context, update PolicyUpdate) (Room, error) {
	wire := &adminpb.UpdateRoomPolicyRequest{
		Tenant:     r.client.tenant,
		Room:       r.name,
		Policy:     policyToProto(update.Policy),
		UpdateMask: update.Fields,
		Actor:      r.client.actorFor(update.Actor),
	}
	room, err := call(ctx, r.client, "Room.UpdatePolicy", func(ctx context.Context) (*adminpb.Room, error) {
		return r.client.rooms.UpdateRoomPolicy(ctx, wire)
	})
	return roomFromProto(room), err
}

// Close ends the room for everyone.
//
// Every session is ejected with the given code, so clients can say why rather than
// going dark. Idempotent: closing a closed room returns it unchanged.
func (r *RoomHandle) Close(ctx context.Context, code ModerationCode, actor Actor) (Room, error) {
	wire := &adminpb.CloseRoomRequest{
		Tenant: r.client.tenant,
		Room:   r.name,
		Code:   codeToProto(code),
		Actor:  r.client.actorFor(actor),
	}
	room, err := call(ctx, r.client, "Room.Close", func(ctx context.Context) (*adminpb.Room, error) {
		return r.client.rooms.CloseRoom(ctx, wire)
	})
	return roomFromProto(room), err
}

// ParticipantQuery filters a roster listing.
type ParticipantQuery struct {
	// IncludeBackstage includes participants who are backstage. Excluded by
	// default, because the common question is "who is in the room" and backstage
	// deliberately is not.
	IncludeBackstage bool

	// IncludeDeparted includes participants who have left but whose record is
	// still within retention. Useful for "who was here when this happened".
	IncludeDeparted bool

	PageSize  uint32
	PageToken string
}

// Participants lists the roster.
func (r *RoomHandle) Participants(ctx context.Context, query ParticipantQuery) (Page[Participant], error) {
	wire := &adminpb.ListParticipantsRequest{
		Tenant:           r.client.tenant,
		Room:             r.name,
		IncludeBackstage: query.IncludeBackstage,
		IncludeDeparted:  query.IncludeDeparted,
		PageSize:         query.PageSize,
		PageToken:        query.PageToken,
	}
	response, err := call(ctx, r.client, "Room.Participants", func(ctx context.Context) (*adminpb.ListParticipantsResponse, error) {
		return r.client.rooms.ListParticipants(ctx, wire)
	})
	if err != nil {
		return Page[Participant]{}, err
	}

	page := Page[Participant]{Next: response.GetNextPageToken()}
	for _, participant := range response.GetParticipants() {
		page.Items = append(page.Items, participantFromProto(participant))
	}
	return page, nil
}

// Participant reads one device's state.
//
// A device is required. Asking about a person rather than a device is Participants
// with a filter, because the answer is a list.
func (r *RoomHandle) Participant(ctx context.Context, identity Identity) (Participant, error) {
	if identity.Username == "" || identity.Device == "" {
		return Participant{}, errors.New("moqom: Participant needs a username and a device")
	}
	participant, err := call(ctx, r.client, "Room.Participant", func(ctx context.Context) (*adminpb.Participant, error) {
		return r.client.rooms.GetParticipant(ctx, &adminpb.GetParticipantRequest{
			Tenant:   r.client.tenant,
			Room:     r.name,
			Identity: identityToProto(identity),
		})
	})
	return participantFromProto(participant), err
}

// NewWatchdog is the request to attach an observer.
type NewWatchdog struct {
	Kind WatchdogKind

	// CustomKind is required when Kind is WatchdogCustom.
	CustomKind string

	OperatedBy WatchdogOperator

	// OperatorName is required when OperatedBy is OperatedByThirdParty: a room
	// that discloses it is being watched should be able to say by whom.
	OperatorName string

	// Descriptor is shown to clients, so write it for a person.
	Descriptor string

	// Scope is the namespace prefix to observe. Empty for the whole room.
	Scope string

	// PinnedLayer subscribes to one rung only. Set it for a classifier: 360p is
	// usually enough, and taking 1080p because nothing stopped it is how
	// observation gets expensive.
	PinnedLayer string

	// IsBillable is whether the fan-out is charged to the tenant.
	IsBillable bool

	Actor Actor
}

// AttachWatchdog attaches an observer that is not a participant.
func (r *RoomHandle) AttachWatchdog(ctx context.Context, request NewWatchdog) (Watchdog, error) {
	wire := &adminpb.AttachWatchdogRequest{
		Tenant:       r.client.tenant,
		Room:         r.name,
		Kind:         watchdogKindToProto(request.Kind),
		CustomKind:   request.CustomKind,
		OperatedBy:   operatorToProto(request.OperatedBy),
		OperatorName: request.OperatorName,
		Descriptor_:  request.Descriptor,
		Scope:        request.Scope,
		PinnedLayer:  request.PinnedLayer,
		IsBillable:   request.IsBillable,
		Actor:        r.client.actorFor(request.Actor),
	}
	watchdog, err := call(ctx, r.client, "Room.AttachWatchdog", func(ctx context.Context) (*adminpb.Watchdog, error) {
		return r.client.rooms.AttachWatchdog(ctx, wire)
	})
	return watchdogFromProto(watchdog), err
}

// DetachWatchdog removes an observer.
func (r *RoomHandle) DetachWatchdog(ctx context.Context, id string, actor Actor) (Watchdog, error) {
	wire := &adminpb.DetachWatchdogRequest{
		Tenant:     r.client.tenant,
		Room:       r.name,
		WatchdogId: id,
		Actor:      r.client.actorFor(actor),
	}
	watchdog, err := call(ctx, r.client, "Room.DetachWatchdog", func(ctx context.Context) (*adminpb.Watchdog, error) {
		return r.client.rooms.DetachWatchdog(ctx, wire)
	})
	return watchdogFromProto(watchdog), err
}

// Watchdogs lists what is observing the room.
func (r *RoomHandle) Watchdogs(ctx context.Context) ([]Watchdog, error) {
	response, err := call(ctx, r.client, "Room.Watchdogs", func(ctx context.Context) (*adminpb.ListWatchdogsResponse, error) {
		return r.client.rooms.ListWatchdogs(ctx, &adminpb.ListWatchdogsRequest{
			Tenant: r.client.tenant,
			Room:   r.name,
		})
	})
	if err != nil {
		return nil, err
	}
	watchdogs := make([]Watchdog, 0, len(response.GetWatchdogs()))
	for _, watchdog := range response.GetWatchdogs() {
		watchdogs = append(watchdogs, watchdogFromProto(watchdog))
	}
	return watchdogs, nil
}
