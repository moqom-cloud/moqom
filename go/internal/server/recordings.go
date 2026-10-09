package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	adminpb "github.com/moqom-cloud/moqom-go/admin/adminpb"
	"github.com/moqom-cloud/moqom/go/internal/control"
	"github.com/moqom-cloud/moqom/go/internal/recorderpb"
	"github.com/moqom-cloud/moqom/go/internal/recording"
	"github.com/moqom-cloud/moqom/go/internal/store"
	"github.com/moqom-cloud/moqom/go/internal/token"
)

// RecorderSubject is the username every recorder token carries.
//
// Reserved: MintClientToken refuses it, so a participant cannot be named it.
// That and the recorder's own key are what let the control plane leave the
// recorder's relay sessions off the roster without trusting anything a client
// says -- a session is the recorder only if it is this name on the device the
// recorder's key derives.
const RecorderSubject = "moqom-recorder"

// RecordingConfig turns on hosted recording. Nil leaves it off, and the
// Recordings service then refuses every call with FAILED_PRECONDITION.
type RecordingConfig struct {
	Store recording.Store

	// Bucket is where every tenant's recordings go, each under a prefix that
	// begins with the tenant.
	Bucket string

	// RecorderThumbprint is the JWK thumbprint of the recorder's key. Every
	// recorder token is bound to it, so a token taken off the link is useless
	// to anything that does not hold the key.
	RecorderThumbprint string

	// Signer makes download links. Nil leaves them out of GetRecording.
	Signer recording.URLSigner

	// Meter is told what each attempt stored. Nil logs it.
	Meter recording.StorageMeter

	// Lease is how long a worker holds a recording without renewing.
	Lease time.Duration
}

const (
	defaultRecorderLease = 45 * time.Second
	defaultURLLifetime   = 15 * time.Minute
	// maxListedArtifacts bounds a GetRecording answer; the manifest lists the
	// rest.
	maxListedArtifacts = 1000
)

type recordingCore struct {
	store      recording.Store
	bucket     string
	thumbprint string
	device     string
	signer     recording.URLSigner
	meter      recording.StorageMeter
	lease      time.Duration
}

func newRecordingCore(config *RecordingConfig) (*recordingCore, error) {
	if config == nil {
		return nil, nil
	}
	if config.Store == nil || config.Bucket == "" || config.RecorderThumbprint == "" {
		return nil, errors.New("hosted recording needs a store, a bucket, and the recorder's key thumbprint")
	}
	device, err := control.DeviceFromThumbprint(config.RecorderThumbprint)
	if err != nil {
		return nil, fmt.Errorf("recorder thumbprint: %w", err)
	}
	meter := config.Meter
	if meter == nil {
		meter = recording.LogMeter{}
	}
	lease := config.Lease
	if lease == 0 {
		lease = defaultRecorderLease
	}
	return &recordingCore{
		store:      config.Store,
		bucket:     config.Bucket,
		thumbprint: config.RecorderThumbprint,
		device:     device,
		signer:     config.Signer,
		meter:      meter,
		lease:      lease,
	}, nil
}

// isRecorder reports whether a relay session is a hosted recorder's.
func (c *core) isRecorder(identity control.Identity) bool {
	return c.recording != nil && identity.Username == RecorderSubject && identity.Device == c.recording.device
}

var recorderAccount = ServiceAccount{Name: RecorderSubject}

var recordingStateToProto = map[recording.State]adminpb.RecordingState{
	recording.StatePending:   adminpb.RecordingState_RECORDING_STATE_PENDING,
	recording.StateRecording: adminpb.RecordingState_RECORDING_STATE_RECORDING,
	recording.StateStopping:  adminpb.RecordingState_RECORDING_STATE_STOPPING,
	recording.StateCompleted: adminpb.RecordingState_RECORDING_STATE_COMPLETED,
	recording.StateFailed:    adminpb.RecordingState_RECORDING_STATE_FAILED,
}

func recordingToProto(r recording.Recording) *adminpb.Recording {
	format := adminpb.RecordingFormat_RECORDING_FORMAT_ARCHIVE_AND_CMAF
	if r.Format == recording.FormatArchive {
		format = adminpb.RecordingFormat_RECORDING_FORMAT_ARCHIVE
	}
	options := &adminpb.RecordingOptions{Format: format}
	if r.MaxDuration > 0 {
		options.MaxDuration = durationpb.New(r.MaxDuration)
	}
	totals := r.Totals()
	wire := &adminpb.Recording{
		Id:          r.ID,
		Tenant:      r.Tenant,
		Room:        r.Room,
		State:       recordingStateToProto[r.State],
		Options:     options,
		Encrypted:   r.Encrypted,
		StorageUri:  "gs://" + r.Bucket + "/" + r.Prefix,
		WatchdogId:  r.WatchdogID,
		Automatic:   r.Automatic,
		CreatedAt:   timeToProto(r.CreatedAt),
		StartedAt:   timeToProto(r.StartedAt),
		EndedAt:     timeToProto(r.EndedAt),
		BytesStored: totals.BytesStored,
		Objects:     totals.Objects,
		Error:       r.Error,
	}
	for i, artifact := range r.Artifacts {
		if i == maxListedArtifacts {
			wire.ArtifactsTruncated = true
			break
		}
		wire.Artifacts = append(wire.Artifacts, &adminpb.RecordingArtifact{
			Name:  artifact.Name,
			Kind:  artifact.Kind,
			Track: artifact.Track,
			Bytes: artifact.Bytes,
		})
	}
	return wire
}

// prefixFor is where a recording's files go: the tenant first, so a grant on
// one tenant's prefix is a grant on nothing else, then the room and the
// recording. Escaped, so a room name cannot add path segments of its own.
func prefixFor(tenant, room, id string) string {
	return url.PathEscape(tenant) + "/" + url.PathEscape(room) + "/" + id
}

// startRecording attaches the watchdog and queues the recording for a worker.
//
// The watchdog goes first. If queuing then fails the room has been told about
// a recording that never happens, which is detached below; the other order
// could have a recorder in the room before anybody had been told.
func (c *core) startRecording(
	ctx context.Context,
	account ServiceAccount,
	actor *adminpb.ActorContext,
	room control.Room,
	options *adminpb.RecordingOptions,
	automatic bool,
) (recording.Recording, error) {
	if !room.IsOpen() {
		return recording.Recording{}, fmt.Errorf("%w: %s has ended", control.ErrRoomClosed, room.Room)
	}
	policy := room.Policy.WithDefaults()
	if policy.Recording == control.RecordingProhibited {
		return recording.Recording{}, status.Error(codes.FailedPrecondition, "this room's policy prohibits recording")
	}
	active, err := c.recording.store.Active(ctx, room.Tenant, room.Room)
	if err != nil {
		return recording.Recording{}, err
	}
	if len(active) > 0 {
		return recording.Recording{}, recording.ErrActive
	}

	format := recording.FormatArchiveAndCMAF
	if options.GetFormat() == adminpb.RecordingFormat_RECORDING_FORMAT_ARCHIVE {
		format = recording.FormatArchive
	}
	maxDuration := options.GetMaxDuration().AsDuration()
	if maxDuration < 0 {
		return recording.Recording{}, fmt.Errorf("%w: max_duration is negative", control.ErrInvalid)
	}

	watchdog, err := c.store.AttachWatchdog(ctx, room.Tenant, room.Room, control.Watchdog{
		Kind:         control.WatchdogRecording,
		OperatedBy:   control.OperatorPlatform,
		OperatorName: "MOQOM",
		Descriptor:   "hosted recorder",
		Scope:        room.Namespace(),
		Health:       control.WatchdogAttached,
		AttachedAt:   c.now(),
		Billable:     true,
	})
	if err != nil {
		return recording.Recording{}, err
	}

	id := c.ids()
	created, err := c.recording.store.Create(ctx, recording.Recording{
		ID:          id,
		Tenant:      room.Tenant,
		Room:        room.Room,
		State:       recording.StatePending,
		Format:      format,
		MaxDuration: maxDuration,
		Automatic:   automatic,
		Encrypted:   true,
		Bucket:      c.recording.bucket,
		Prefix:      prefixFor(room.Tenant, room.Room, id),
		WatchdogID:  watchdog.ID,
		CreatedAt:   c.now(),
	})
	if err != nil {
		if _, detachErr := c.store.DetachWatchdog(ctx, room.Tenant, room.Room, watchdog.ID); detachErr != nil {
			slog.WarnContext(ctx, "could not withdraw a recording watchdog", "room", room.Room, "error", detachErr)
		}
		return recording.Recording{}, err
	}

	if _, err := c.record(ctx, account, actor, control.ModerationEntry{
		Tenant: room.Tenant,
		Room:   room.Room,
		Verb:   control.VerbAttachWatchdog,
	}, true); err != nil {
		return recording.Recording{}, err
	}
	c.publish(ctx, room.Tenant, room.Room, store.EventWatchdogChanged, &adminpb.RoomEvent{
		Kind: adminpb.RoomEventKind_ROOM_EVENT_KIND_WATCHDOG_CHANGED,
		Body: &adminpb.RoomEvent_WatchdogChanged{WatchdogChanged: &adminpb.WatchdogChanged{
			Watchdog: watchdogToProto(watchdog),
		}},
	})
	return created, nil
}

// endRecording withdraws a finished recording's watchdog, so the room stops
// saying it is being recorded once it is not.
func (c *core) endRecording(ctx context.Context, r recording.Recording) {
	if r.WatchdogID == "" {
		return
	}
	detached, err := c.store.DetachWatchdog(ctx, r.Tenant, r.Room, r.WatchdogID)
	if err != nil {
		if !errors.Is(err, control.ErrNotFound) {
			slog.WarnContext(ctx, "could not detach a recording watchdog", "recording", r.ID, "error", err)
		}
		return
	}
	if _, err := c.record(ctx, recorderAccount, nil, control.ModerationEntry{
		Tenant: r.Tenant,
		Room:   r.Room,
		Verb:   control.VerbDetachWatchdog,
	}, true); err != nil {
		slog.WarnContext(ctx, "could not log a recording ending", "recording", r.ID, "error", err)
	}
	c.publish(ctx, r.Tenant, r.Room, store.EventWatchdogChanged, &adminpb.RoomEvent{
		Kind: adminpb.RoomEventKind_ROOM_EVENT_KIND_WATCHDOG_CHANGED,
		Body: &adminpb.RoomEvent_WatchdogChanged{WatchdogChanged: &adminpb.WatchdogChanged{
			Watchdog: watchdogToProto(detached),
			Detached: true,
		}},
	})
}

// requestStop asks a recording to stop. One no worker has picked up ends at
// once, with nothing recorded; one being recorded ends when its worker
// finishes.
func (c *core) requestStop(ctx context.Context, id string) (recording.Recording, error) {
	ended := false
	updated, err := c.recording.store.Update(ctx, id, func(r *recording.Recording) error {
		ended = false
		if r.State.Terminal() {
			return nil
		}
		r.StopRequested = true
		if r.Worker == "" && r.State == recording.StatePending {
			r.State = recording.StateCompleted
			r.EndedAt = c.now()
			ended = true
			return nil
		}
		r.State = recording.StateStopping
		return nil
	})
	if err != nil {
		return recording.Recording{}, err
	}
	if ended {
		c.endRecording(ctx, updated)
	}
	return updated, nil
}

// followRecordingPolicy is what a room's recording policy means once the room
// exists: required starts a recording, prohibited stops any that are running.
//
// Called after the room is saved, and never fails the call that saved it:
// the room's state is already what the caller asked for, and the recording
// follows it. A required recording that could not start is logged and tried
// again by the next policy write; a recorder pool that is down leaves the
// recording pending, which is visible in ListRecordings.
func (c *core) followRecordingPolicy(ctx context.Context, account ServiceAccount, actor *adminpb.ActorContext, room control.Room) {
	if c.recording == nil {
		return
	}
	switch room.Policy.WithDefaults().Recording {
	case control.RecordingRequired:
		if !room.IsOpen() {
			return
		}
		if _, err := c.startRecording(ctx, account, actor, room, nil, true); err != nil && !errors.Is(err, recording.ErrActive) {
			slog.WarnContext(ctx, "a room that requires recording could not start one",
				"tenant", room.Tenant, "room", room.Room, "error", err)
		}
	case control.RecordingProhibited:
		c.stopRecordings(ctx, room.Tenant, room.Room)
	}
}

// stopRecordings asks every recording of a room to stop, as the room closes
// or its policy forbids them.
func (c *core) stopRecordings(ctx context.Context, tenant, room string) {
	if c.recording == nil {
		return
	}
	active, err := c.recording.store.Active(ctx, tenant, room)
	if err != nil {
		slog.WarnContext(ctx, "could not find a room's recordings to stop", "room", room, "error", err)
		return
	}
	for _, r := range active {
		if _, err := c.requestStop(ctx, r.ID); err != nil {
			slog.WarnContext(ctx, "could not stop a recording", "recording", r.ID, "error", err)
		}
	}
}

type recordingsService struct {
	adminpb.UnimplementedRecordingsServer
	core *core
}

func (s *recordingsService) enabled() error {
	if s.core.recording == nil {
		return status.Error(codes.FailedPrecondition, "hosted recording is not enabled on this deployment")
	}
	return nil
}

func (s *recordingsService) StartRecording(ctx context.Context, request *adminpb.StartRecordingRequest) (*adminpb.Recording, error) {
	account, err := requireTenant(ctx, request.GetTenant())
	if err != nil {
		return nil, err
	}
	if err := s.enabled(); err != nil {
		return nil, err
	}
	// A recording is a watchdog, and is authorised as attaching one.
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
	started, err := s.core.startRecording(ctx, account, request.GetActor(), room, request.GetOptions(), false)
	if err != nil {
		if _, ok := status.FromError(err); ok {
			return nil, err
		}
		return nil, statusFor(err)
	}
	return recordingToProto(started), nil
}

func (s *recordingsService) StopRecording(ctx context.Context, request *adminpb.StopRecordingRequest) (*adminpb.Recording, error) {
	account, err := requireTenant(ctx, request.GetTenant())
	if err != nil {
		return nil, err
	}
	if err := s.enabled(); err != nil {
		return nil, err
	}
	found, err := s.core.recording.store.Get(ctx, request.GetTenant(), request.GetRecordingId())
	if err != nil {
		return nil, statusFor(err)
	}
	actor, err := s.core.effectiveActor(ctx, account, found.Tenant, found.Room, request.GetActor())
	if err != nil {
		return nil, statusFor(err)
	}
	if err := control.Authorise(actor, control.VerbDetachWatchdog, nil); err != nil {
		return nil, statusFor(err)
	}
	if found.State.Terminal() {
		return recordingToProto(found), nil
	}
	room, err := s.core.store.GetRoom(ctx, found.Tenant, found.Room)
	if err != nil && !errors.Is(err, control.ErrNotFound) {
		return nil, statusFor(err)
	}
	if err == nil && room.IsOpen() && room.Policy.WithDefaults().Recording == control.RecordingRequired {
		return nil, status.Error(codes.FailedPrecondition,
			"this room's policy requires recording; change the policy or close the room to stop it")
	}
	stopped, err := s.core.requestStop(ctx, found.ID)
	if err != nil {
		return nil, statusFor(err)
	}
	return recordingToProto(stopped), nil
}

func (s *recordingsService) ListRecordings(ctx context.Context, request *adminpb.ListRecordingsRequest) (*adminpb.ListRecordingsResponse, error) {
	if _, err := requireTenant(ctx, request.GetTenant()); err != nil {
		return nil, err
	}
	if err := s.enabled(); err != nil {
		return nil, err
	}
	page, err := s.core.recording.store.List(ctx, recording.Query{
		Tenant:    request.GetTenant(),
		Room:      request.GetRoom(),
		PageSize:  request.GetPageSize(),
		PageToken: request.GetPageToken(),
	})
	if err != nil {
		return nil, statusFor(err)
	}
	response := &adminpb.ListRecordingsResponse{NextPageToken: page.NextPageToken}
	for _, r := range page.Recordings {
		response.Recordings = append(response.Recordings, recordingToProto(r))
	}
	return response, nil
}

func (s *recordingsService) GetRecording(ctx context.Context, request *adminpb.GetRecordingRequest) (*adminpb.Recording, error) {
	if _, err := requireTenant(ctx, request.GetTenant()); err != nil {
		return nil, err
	}
	if err := s.enabled(); err != nil {
		return nil, err
	}
	found, err := s.core.recording.store.Get(ctx, request.GetTenant(), request.GetRecordingId())
	if err != nil {
		return nil, statusFor(err)
	}
	wire := recordingToProto(found)

	signer := s.core.recording.signer
	if signer == nil {
		return wire, nil
	}
	lifetime := defaultURLLifetime
	if request.GetUrlLifetime() != nil {
		lifetime = request.GetUrlLifetime().AsDuration()
	}
	if lifetime <= 0 || lifetime > recording.MaxURLLifetime {
		return nil, status.Errorf(codes.InvalidArgument, "url_lifetime must be between one second and %s", recording.MaxURLLifetime)
	}
	now := s.core.now()
	for _, artifact := range wire.Artifacts {
		link, err := signer.SignedURL(ctx, found.Bucket, found.Prefix+"/"+artifact.GetName(), lifetime, now)
		if err != nil {
			return nil, status.Errorf(codes.Unavailable, "signing download links: %v", err)
		}
		artifact.DownloadUrl = link
	}
	if n := len(found.Manifests); n > 0 {
		link, err := signer.SignedURL(ctx, found.Bucket, found.Prefix+"/"+found.Manifests[n-1], lifetime, now)
		if err != nil {
			return nil, status.Errorf(codes.Unavailable, "signing download links: %v", err)
		}
		wire.ManifestUrl = link
	}
	wire.UrlsExpireAt = timeToProto(now.Add(lifetime))
	return wire, nil
}

// The recorder link.

func requireRecorder(ctx context.Context) (ServiceAccount, error) {
	account, ok := PrincipalFrom(ctx)
	if !ok {
		return ServiceAccount{}, status.Error(codes.Unauthenticated, "no principal")
	}
	if !account.IsRecorder {
		return ServiceAccount{}, status.Error(codes.PermissionDenied, "this service is for hosted recorders")
	}
	return account, nil
}

type recorderLinkService struct {
	recorderpb.UnimplementedRecorderLinkServer
	core *core
}

// credential mints the recorder's relay token for a recording: a viewer of
// the room, nothing more, bound to the recorder's key.
func (s *recorderLinkService) credential(ctx context.Context, r recording.Recording, room control.Room) (*recorderpb.Credential, time.Time, error) {
	claims := token.Claims{
		Issuer:       s.core.issuer,
		Subject:      RecorderSubject,
		Tenant:       r.Tenant,
		Confirmation: token.Confirmation{Thumbprint: s.core.recording.thumbprint},
		Grant:        token.ScopeFrom(control.Grants{}, r.Room, false, false),
	}
	if _, err := s.core.confine(ctx, &claims); err != nil {
		return nil, time.Time{}, err
	}
	minted, expires, err := s.core.mint(claims, token.DefaultLifetime)
	if err != nil {
		return nil, time.Time{}, err
	}
	return &recorderpb.Credential{Token: minted, ExpiresAt: timeToProto(expires)}, expires, nil
}

// unrecordable says why a recording should not continue in this room, or ""
// if it may.
func unrecordable(room control.Room, err error) string {
	switch {
	case errors.Is(err, control.ErrNotFound):
		return "the room no longer exists"
	case err != nil:
		return ""
	case !room.IsOpen():
		return "the room ended"
	case room.Policy.WithDefaults().Recording == control.RecordingProhibited:
		return "the room's policy now prohibits recording"
	}
	return ""
}

func (s *recorderLinkService) Claim(ctx context.Context, request *recorderpb.ClaimRequest) (*recorderpb.ClaimResponse, error) {
	if _, err := requireRecorder(ctx); err != nil {
		return nil, err
	}
	if s.core.recording == nil {
		return nil, status.Error(codes.FailedPrecondition, "hosted recording is not enabled on this deployment")
	}
	if request.GetWorker() == "" {
		return nil, status.Error(codes.InvalidArgument, "worker is required")
	}
	// A few rounds, because a claimed recording whose room has since ended is
	// closed here rather than handed out, and the next one may be fine.
	for range 4 {
		claimed, ok, err := s.core.recording.store.Claim(ctx, request.GetWorker(), s.core.now(), s.core.recording.lease)
		if err != nil {
			return nil, statusFor(err)
		}
		if !ok {
			return &recorderpb.ClaimResponse{}, nil
		}
		room, roomErr := s.core.store.GetRoom(ctx, claimed.Tenant, claimed.Room)
		if roomErr != nil && !errors.Is(roomErr, control.ErrNotFound) {
			return nil, statusFor(roomErr)
		}
		if why := unrecordable(room, roomErr); why != "" || claimed.StopRequested {
			ended, err := s.core.recording.store.Update(ctx, claimed.ID, func(r *recording.Recording) error {
				r.Worker, r.LeaseUntil = "", time.Time{}
				r.State = recording.StateCompleted
				r.EndedAt = s.core.now()
				return nil
			})
			if err != nil {
				return nil, statusFor(err)
			}
			s.core.endRecording(ctx, ended)
			continue
		}

		credential, expires, err := s.credential(ctx, claimed, room)
		if err != nil {
			return nil, statusFor(err)
		}
		claimed, err = s.core.recording.store.Update(ctx, claimed.ID, func(r *recording.Recording) error {
			r.CredentialExpires = expires
			r.Encrypted = true
			return nil
		})
		if err != nil {
			return nil, statusFor(err)
		}
		assignment := &recorderpb.Assignment{
			RecordingId:   claimed.ID,
			Tenant:        claimed.Tenant,
			Room:          claimed.Room,
			StorageBucket: claimed.Bucket,
			StoragePrefix: claimed.Prefix,
			E2EeRequired:  true,
			Remux:         claimed.Format.Remux(),
			Credential:    credential,
			Attempt:       claimed.Attempt,
			LeaseSeconds:  uint32(s.core.recording.lease / time.Second),
		}
		if deadline := claimed.Deadline(); !deadline.IsZero() {
			assignment.Deadline = timeToProto(deadline)
		}
		return &recorderpb.ClaimResponse{Assignment: assignment}, nil
	}
	return &recorderpb.ClaimResponse{}, nil
}

func progressFromProto(wire *recorderpb.Progress) recording.Progress {
	return recording.Progress{
		Objects:       wire.GetObjects(),
		BytesReceived: wire.GetBytesReceived(),
		BytesStored:   wire.GetBytesStored(),
		Tracks:        wire.GetTracks(),
		Reconnects:    wire.GetReconnects(),
	}
}

// leaseLost is how a worker learns another worker has the recording now.
var leaseLost = status.Error(codes.FailedPrecondition, recording.ErrLeaseLost.Error())

func (s *recorderLinkService) Renew(ctx context.Context, request *recorderpb.RenewRequest) (*recorderpb.RenewResponse, error) {
	if _, err := requireRecorder(ctx); err != nil {
		return nil, err
	}
	if s.core.recording == nil {
		return nil, status.Error(codes.FailedPrecondition, "hosted recording is not enabled on this deployment")
	}
	now := s.core.now()
	var room control.Room
	var roomErr error
	renewed, err := s.core.recording.store.Update(ctx, request.GetRecordingId(), func(r *recording.Recording) error {
		if r.Worker != request.GetWorker() || r.State.Terminal() {
			return recording.ErrLeaseLost
		}
		room, roomErr = s.core.store.GetRoom(ctx, r.Tenant, r.Room)
		if why := unrecordable(room, roomErr); why != "" {
			r.StopRequested = true
		}
		if deadline := r.Deadline(); !deadline.IsZero() && !now.Before(deadline) {
			r.StopRequested = true
		}
		if r.StopRequested {
			r.State = recording.StateStopping
		}
		r.LeaseUntil = now.Add(s.core.recording.lease)
		r.Current = progressFromProto(request.GetProgress())
		return nil
	})
	if errors.Is(err, recording.ErrLeaseLost) {
		return nil, leaseLost
	}
	if err != nil {
		return nil, statusFor(err)
	}

	response := &recorderpb.RenewResponse{Stop: renewed.StopRequested}
	// A fresh token once the one held is half spent, so the worker always has
	// minutes in hand to pass it to the relay.
	if !response.Stop && roomErr == nil && renewed.CredentialExpires.Sub(now) < token.DefaultLifetime/2 {
		credential, expires, err := s.credential(ctx, renewed, room)
		if err != nil {
			return nil, statusFor(err)
		}
		if _, err := s.core.recording.store.Update(ctx, renewed.ID, func(r *recording.Recording) error {
			r.CredentialExpires = expires
			return nil
		}); err != nil {
			return nil, statusFor(err)
		}
		response.Credential = credential
	}
	return response, nil
}

func (s *recorderLinkService) Finish(ctx context.Context, request *recorderpb.FinishRequest) (*recorderpb.FinishResponse, error) {
	if _, err := requireRecorder(ctx); err != nil {
		return nil, err
	}
	if s.core.recording == nil {
		return nil, status.Error(codes.FailedPrecondition, "hosted recording is not enabled on this deployment")
	}
	now := s.core.now()
	var stored uint64
	var attempt uint32
	finished, err := s.core.recording.store.Update(ctx, request.GetRecordingId(), func(r *recording.Recording) error {
		if r.Worker != request.GetWorker() || r.State.Terminal() {
			return recording.ErrLeaseLost
		}
		attempt = r.Attempt
		stored = 0
		for _, artifact := range request.GetArtifacts() {
			r.Artifacts = append(r.Artifacts, recording.Artifact{
				Name:     artifact.GetName(),
				Kind:     artifact.GetKind(),
				Track:    artifact.GetTrack(),
				Bytes:    artifact.GetBytes(),
				CRC32C:   artifact.GetCrc32C(),
				Attempt:  r.Attempt,
				StoredAt: now,
			})
			stored += artifact.GetBytes()
		}
		if request.GetManifest() != "" {
			r.Manifests = append(r.Manifests, request.GetManifest())
		}
		r.Stored = r.Stored.Add(progressFromProto(request.GetProgress()))
		r.Current = recording.Progress{}
		r.Worker, r.LeaseUntil = "", time.Time{}

		if request.GetRequeue() && !r.StopRequested {
			// The worker is going, the recording is not: the next claim
			// resumes it under the next attempt.
			return nil
		}
		r.EndedAt = now
		r.State = recording.StateCompleted
		if request.GetError() != "" {
			r.State = recording.StateFailed
			r.Error = request.GetError()
		}
		return nil
	})
	if errors.Is(err, recording.ErrLeaseLost) {
		return nil, leaseLost
	}
	if err != nil {
		return nil, statusFor(err)
	}

	if err := s.core.recording.meter.Stored(ctx, recording.StorageUsage{
		Tenant:    finished.Tenant,
		Room:      finished.Room,
		Recording: finished.ID,
		Attempt:   attempt,
		Bytes:     stored,
		Artifacts: len(request.GetArtifacts()),
		At:        now,
	}); err != nil {
		slog.WarnContext(ctx, "could not meter recording storage", "recording", finished.ID, "error", err)
	}
	if finished.State.Terminal() {
		s.core.endRecording(ctx, finished)
	}
	return &recorderpb.FinishResponse{}, nil
}
