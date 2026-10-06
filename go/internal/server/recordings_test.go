package server_test

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/durationpb"

	adminpb "github.com/moqom-cloud/moqom-go/admin/adminpb"
	"github.com/moqom-cloud/moqom/go/internal/control"
	"github.com/moqom-cloud/moqom/go/internal/recorderpb"
	"github.com/moqom-cloud/moqom/go/internal/recording"
	relaypb "github.com/moqom-cloud/moqom/go/internal/relaypb"
	"github.com/moqom-cloud/moqom/go/internal/server"
	"github.com/moqom-cloud/moqom/go/internal/store/memory"
	"github.com/moqom-cloud/moqom/go/internal/token"
)

const (
	recorderKey        = "recorder-key"
	otherTenantKey     = "globex-key"
	recordingBucket    = "moqom-recordings-test"
	recorderThumbprint = "recorderrecorderrecorderrecorder0123456789a"
	recorderDevice     = "recorderrecorder"
	recorderLease      = 45 * time.Second
)

type fakeSigner struct{}

func (fakeSigner) SignedURL(_ context.Context, bucket, object string, lifetime time.Duration, _ time.Time) (string, error) {
	return "https://signed.example/" + bucket + "/" + object + "?for=" + lifetime.String(), nil
}

type fakeMeter struct {
	mu   sync.Mutex
	used []recording.StorageUsage
}

func (m *fakeMeter) Stored(_ context.Context, usage recording.StorageUsage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.used = append(m.used, usage)
	return nil
}

func (m *fakeMeter) total() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var bytes uint64
	for _, usage := range m.used {
		bytes += usage.Bytes
	}
	return bytes
}

type recordingHarness struct {
	*harness
	recordings adminpb.RecordingsClient
	link       recorderpb.RecorderLinkClient
	meter      *fakeMeter
}

func dial(t *testing.T, listener *bufconn.Listener) *grpc.ClientConn {
	t.Helper()
	connection, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dialling: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return connection
}

// newRecordingHarness is the control plane with hosted recording on.
func newRecordingHarness(t *testing.T) *recordingHarness {
	t.Helper()

	current := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	now := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return current
	}
	advance := func(d time.Duration) {
		clockMu.Lock()
		defer clockMu.Unlock()
		current = current.Add(d)
	}

	memoryStore := memory.New(now, nil)
	signing, err := token.GenerateKey()
	if err != nil {
		t.Fatalf("generating a signing key: %v", err)
	}
	accounts := server.NewStaticAccounts(map[string]server.ServiceAccount{
		ownerKey: {Name: "owner-backend", Tenant: tenant, Grants: control.Grants{
			Publish: control.PublishFull, Moderation: control.CapAll, IsOwner: true,
		}},
		joinKey: {Name: "join-backend", Tenant: tenant, Grants: control.Grants{
			Publish: control.PublishCamera,
		}},
		otherTenantKey: {Name: "globex-backend", Tenant: "globex", Grants: control.Grants{
			Publish: control.PublishFull, Moderation: control.CapAll, IsOwner: true,
		}},
		relayKey:    {Name: "relay-1", IsRelay: true},
		recorderKey: {Name: "recorders", IsRecorder: true},
	})
	meter := &fakeMeter{}

	instance, err := server.New(server.Config{
		Store:           memoryStore,
		Accounts:        accounts,
		Issuer:          issuerFor,
		Keys:            []token.Key{signing},
		Now:             now,
		DispatchTimeout: 2 * time.Second,
		Recording: &server.RecordingConfig{
			Store:              recording.NewMemory(),
			Bucket:             recordingBucket,
			RecorderThumbprint: recorderThumbprint,
			Signer:             fakeSigner{},
			Meter:              meter,
			Lease:              recorderLease,
		},
	})
	if err != nil {
		t.Fatalf("building the control plane: %v", err)
	}
	unary, streaming := instance.Interceptors()
	grpcServer := grpc.NewServer(grpc.UnaryInterceptor(unary), grpc.StreamInterceptor(streaming))
	instance.Register(grpcServer)
	listener := bufconn.Listen(1 << 20)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(func() {
		grpcServer.Stop()
		_ = listener.Close()
	})
	connection := dial(t, listener)

	return &recordingHarness{
		harness: &harness{
			rooms:      adminpb.NewRoomsClient(connection),
			moderation: adminpb.NewModerationClient(connection),
			tokens:     adminpb.NewTokensClient(connection),
			events:     adminpb.NewEventsClient(connection),
			relay:      relaypb.NewRelayLinkClient(connection),
			store:      memoryStore,
			now:        now,
			advance:    advance,
			server:     instance,
			listener:   listener,
		},
		recordings: adminpb.NewRecordingsClient(connection),
		link:       recorderpb.NewRecorderLinkClient(connection),
		meter:      meter,
	}
}

func (h *recordingHarness) createRoomWith(t *testing.T, name string, policy *adminpb.RoomPolicy) {
	t.Helper()
	if _, err := h.rooms.CreateRoom(as(ownerKey), &adminpb.CreateRoomRequest{
		Tenant: tenant, Room: name, Policy: policy,
		Actor: &adminpb.ActorContext{Reason: "a test that needs it"},
	}); err != nil {
		t.Fatalf("creating %s: %v", name, err)
	}
}

func (h *recordingHarness) start(t *testing.T, room string) *adminpb.Recording {
	t.Helper()
	started, err := h.recordings.StartRecording(as(ownerKey), &adminpb.StartRecordingRequest{Tenant: tenant, Room: room})
	if err != nil {
		t.Fatalf("starting a recording: %v", err)
	}
	return started
}

func (h *recordingHarness) claim(t *testing.T, worker string) *recorderpb.Assignment {
	t.Helper()
	response, err := h.link.Claim(as(recorderKey), &recorderpb.ClaimRequest{Worker: worker})
	if err != nil {
		t.Fatalf("claiming: %v", err)
	}
	return response.GetAssignment()
}

func (h *recordingHarness) get(t *testing.T, id string) *adminpb.Recording {
	t.Helper()
	found, err := h.recordings.GetRecording(as(ownerKey), &adminpb.GetRecordingRequest{Tenant: tenant, RecordingId: id})
	if err != nil {
		t.Fatalf("reading a recording: %v", err)
	}
	return found
}

func (h *recordingHarness) recordingWatchdogs(t *testing.T, room string) int {
	t.Helper()
	watchdogs, err := h.store.ListWatchdogs(context.Background(), tenant, room)
	if err != nil {
		t.Fatalf("listing watchdogs: %v", err)
	}
	count := 0
	for _, watchdog := range watchdogs {
		if watchdog.Kind == control.WatchdogRecording {
			count++
		}
	}
	return count
}

// The whole life of a recording, as a tenant and a worker each see it.
func TestARecordingRunsFromStartToDownload(t *testing.T) {
	h := newRecordingHarness(t)
	h.createRoom(t, "alice")

	started := h.start(t, roomName)
	if started.GetState() != adminpb.RecordingState_RECORDING_STATE_PENDING {
		t.Errorf("a new recording is %v, want PENDING", started.GetState())
	}
	if want := "gs://" + recordingBucket + "/acme/room-42/" + started.GetId(); started.GetStorageUri() != want {
		t.Errorf("storage is %q, want %q under the tenant's prefix", started.GetStorageUri(), want)
	}
	if !started.GetEncrypted() {
		t.Error("a default room is end-to-end encrypted, and its recording should say so")
	}
	if h.recordingWatchdogs(t, roomName) != 1 {
		t.Error("starting a recording should disclose it as a watchdog")
	}
	if _, err := h.recordings.StartRecording(as(ownerKey), &adminpb.StartRecordingRequest{Tenant: tenant, Room: roomName}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("a second recording of the same room gave %v, want AlreadyExists", status.Code(err))
	}

	assignment := h.claim(t, "worker-1")
	if assignment.GetRecordingId() != started.GetId() {
		t.Fatalf("claimed %q, want %q", assignment.GetRecordingId(), started.GetId())
	}
	if assignment.GetStoragePrefix() != "acme/room-42/"+started.GetId() || assignment.GetStorageBucket() != recordingBucket {
		t.Errorf("the worker was told to write to %s/%s", assignment.GetStorageBucket(), assignment.GetStoragePrefix())
	}
	if !assignment.GetE2EeRequired() || !assignment.GetRemux() || assignment.GetAttempt() != 1 {
		t.Errorf("assignment e2ee=%v remux=%v attempt=%d", assignment.GetE2EeRequired(), assignment.GetRemux(), assignment.GetAttempt())
	}
	claims := claimsOf(t, assignment.GetCredential().GetToken())
	if claims["sub"] != server.RecorderSubject || claims["aud"] != tenant {
		t.Errorf("the recorder's token names sub=%v aud=%v", claims["sub"], claims["aud"])
	}
	if cnf, _ := claims["cnf"].(map[string]any); cnf["jkt"] != recorderThumbprint {
		t.Errorf("the recorder's token is bound to %v, not the recorder's key", claims["cnf"])
	}
	scope, _ := claims["moqt"].(map[string]any)
	if scope["room"] != roomName {
		t.Errorf("the recorder's token is for %v", scope["room"])
	}
	if publish, _ := scope["pub"].(float64); publish != 0 {
		t.Errorf("the recorder's token may publish %v; a recorder only watches", publish)
	}
	if again := h.claim(t, "worker-2"); again != nil {
		t.Errorf("a recording under lease was handed out twice, to worker-2 as %s", again.GetRecordingId())
	}

	renewed, err := h.link.Renew(as(recorderKey), &recorderpb.RenewRequest{
		RecordingId: started.GetId(), Worker: "worker-1",
		Progress: &recorderpb.Progress{Objects: 10, BytesReceived: 5000, BytesStored: 4000, Tracks: 2},
	})
	if err != nil || renewed.GetStop() || renewed.GetCredential() != nil {
		t.Fatalf("an early renewal gave %v, stop=%v, fresh token=%v", err, renewed.GetStop(), renewed.GetCredential() != nil)
	}
	if got := h.get(t, started.GetId()); got.GetState() != adminpb.RecordingState_RECORDING_STATE_RECORDING || got.GetObjects() != 10 {
		t.Errorf("a recording in progress reads %v with %d objects", got.GetState(), got.GetObjects())
	}

	// Past half the token's life, the renewal carries a fresh one.
	h.advance(6 * time.Minute)
	renewed, err = h.link.Renew(as(recorderKey), &recorderpb.RenewRequest{RecordingId: started.GetId(), Worker: "worker-1"})
	if err != nil || renewed.GetCredential().GetToken() == "" {
		t.Fatalf("a late renewal gave %v and no fresh token", err)
	}

	stopping, err := h.recordings.StopRecording(as(ownerKey), &adminpb.StopRecordingRequest{Tenant: tenant, RecordingId: started.GetId()})
	if err != nil || stopping.GetState() != adminpb.RecordingState_RECORDING_STATE_STOPPING {
		t.Fatalf("stopping gave %v, state %v", err, stopping.GetState())
	}
	renewed, err = h.link.Renew(as(recorderKey), &recorderpb.RenewRequest{RecordingId: started.GetId(), Worker: "worker-1"})
	if err != nil || !renewed.GetStop() {
		t.Fatalf("the worker was not told to stop: %v", err)
	}

	if _, err := h.link.Finish(as(recorderKey), &recorderpb.FinishRequest{
		RecordingId: started.GetId(), Worker: "worker-1",
		Progress: &recorderpb.Progress{Objects: 20, BytesReceived: 9000, BytesStored: 8000, Tracks: 2},
		Manifest: "manifest-a1.json",
		Artifacts: []*recorderpb.Artifact{
			{Name: "alice/phone/video/a1-s00000.mqa", Kind: "archive", Track: "alice/phone/video", Bytes: 7000, Crc32C: "AAAAAA=="},
			{Name: "manifest-a1.json", Kind: "manifest", Bytes: 1000, Crc32C: "AAAAAA=="},
		},
	}); err != nil {
		t.Fatalf("finishing: %v", err)
	}

	done, err := h.recordings.GetRecording(as(ownerKey), &adminpb.GetRecordingRequest{
		Tenant: tenant, RecordingId: started.GetId(), UrlLifetime: durationpb.New(time.Hour),
	})
	if err != nil {
		t.Fatalf("reading the finished recording: %v", err)
	}
	if done.GetState() != adminpb.RecordingState_RECORDING_STATE_COMPLETED || done.GetBytesStored() != 8000 {
		t.Errorf("a finished recording reads %v, %d bytes", done.GetState(), done.GetBytesStored())
	}
	if len(done.GetArtifacts()) != 2 {
		t.Fatalf("%d artifacts, want 2", len(done.GetArtifacts()))
	}
	wantLink := "https://signed.example/" + recordingBucket + "/acme/room-42/" + started.GetId() + "/alice/phone/video/a1-s00000.mqa?for=1h0m0s"
	if got := done.GetArtifacts()[0].GetDownloadUrl(); got != wantLink {
		t.Errorf("download link %q, want %q", got, wantLink)
	}
	if !strings.HasSuffix(strings.Split(done.GetManifestUrl(), "?")[0], "/manifest-a1.json") {
		t.Errorf("manifest link %q", done.GetManifestUrl())
	}
	if !done.GetUrlsExpireAt().AsTime().Equal(h.now().Add(time.Hour)) {
		t.Errorf("links expire at %v", done.GetUrlsExpireAt().AsTime())
	}
	if h.meter.total() != 8000 {
		t.Errorf("metered %d bytes of storage, want 8000", h.meter.total())
	}
	if h.recordingWatchdogs(t, roomName) != 0 {
		t.Error("a finished recording should withdraw its watchdog")
	}

	listed, err := h.recordings.ListRecordings(as(ownerKey), &adminpb.ListRecordingsRequest{Tenant: tenant, Room: roomName})
	if err != nil || len(listed.GetRecordings()) != 1 {
		t.Fatalf("listing gave %v, %d recordings", err, len(listed.GetRecordings()))
	}
	if listed.GetRecordings()[0].GetArtifacts()[0].GetDownloadUrl() != "" {
		t.Error("listing should not sign links; GetRecording does")
	}
	if _, err := h.link.Finish(as(recorderKey), &recorderpb.FinishRequest{RecordingId: started.GetId(), Worker: "worker-1"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("finishing twice gave %v, want FailedPrecondition", status.Code(err))
	}
}

func TestAProhibitedRoomCannotBeRecorded(t *testing.T) {
	h := newRecordingHarness(t)
	h.createRoomWith(t, "secret", &adminpb.RoomPolicy{Recording: adminpb.RecordingPolicy_RECORDING_POLICY_PROHIBITED})

	_, err := h.recordings.StartRecording(as(ownerKey), &adminpb.StartRecordingRequest{Tenant: tenant, Room: "secret"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("recording a prohibited room gave %v, want FailedPrecondition", status.Code(err))
	}
	if h.recordingWatchdogs(t, "secret") != 0 {
		t.Error("a refused recording left a watchdog behind")
	}
}

func TestARoomThatRequiresRecordingIsRecordedUntilItCloses(t *testing.T) {
	h := newRecordingHarness(t)
	h.createRoomWith(t, "court", &adminpb.RoomPolicy{Recording: adminpb.RecordingPolicy_RECORDING_POLICY_REQUIRED})

	listed, err := h.recordings.ListRecordings(as(ownerKey), &adminpb.ListRecordingsRequest{Tenant: tenant, Room: "court"})
	if err != nil || len(listed.GetRecordings()) != 1 {
		t.Fatalf("a room that requires recording has %d recordings (%v), want one started with it", len(listed.GetRecordings()), err)
	}
	automatic := listed.GetRecordings()[0]
	if !automatic.GetAutomatic() {
		t.Error("the recording a policy started should say so")
	}

	_, err = h.recordings.StopRecording(as(ownerKey), &adminpb.StopRecordingRequest{Tenant: tenant, RecordingId: automatic.GetId()})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("stopping a required recording gave %v, want FailedPrecondition", status.Code(err))
	}

	if _, err := h.rooms.CloseRoom(as(ownerKey), &adminpb.CloseRoomRequest{Tenant: tenant, Room: "court"}); err != nil {
		t.Fatalf("closing: %v", err)
	}
	if got := h.get(t, automatic.GetId()); got.GetState() != adminpb.RecordingState_RECORDING_STATE_COMPLETED {
		t.Errorf("a never-claimed recording of a closed room is %v, want COMPLETED", got.GetState())
	}
	if h.recordingWatchdogs(t, "court") != 0 {
		t.Error("closing the room should have withdrawn the recording's watchdog")
	}
	if assignment := h.claim(t, "worker-1"); assignment != nil {
		t.Errorf("a closed room's recording was handed out: %s", assignment.GetRecordingId())
	}
}

func TestProhibitingRecordingStopsTheRecorder(t *testing.T) {
	h := newRecordingHarness(t)
	h.createRoom(t, "alice")
	started := h.start(t, roomName)
	h.claim(t, "worker-1")

	if _, err := h.rooms.UpdateRoomPolicy(as(ownerKey), &adminpb.UpdateRoomPolicyRequest{
		Tenant: tenant, Room: roomName,
		Policy:     &adminpb.RoomPolicy{Recording: adminpb.RecordingPolicy_RECORDING_POLICY_PROHIBITED},
		UpdateMask: []string{"recording"},
	}); err != nil {
		t.Fatalf("prohibiting recording: %v", err)
	}
	if got := h.get(t, started.GetId()); got.GetState() != adminpb.RecordingState_RECORDING_STATE_STOPPING {
		t.Errorf("after prohibition the recording is %v, want STOPPING", got.GetState())
	}
	renewed, err := h.link.Renew(as(recorderKey), &recorderpb.RenewRequest{RecordingId: started.GetId(), Worker: "worker-1"})
	if err != nil || !renewed.GetStop() {
		t.Errorf("the worker was not told to stop (%v)", err)
	}
}

// A worker that goes away mid-recording, gracefully or not, hands the recording
// on with the attempt counted, so the next worker's files cannot overwrite the
// first's.
func TestARecordingOutlivesItsWorker(t *testing.T) {
	h := newRecordingHarness(t)
	h.createRoom(t, "alice")
	started := h.start(t, roomName)

	h.claim(t, "worker-1")
	if _, err := h.link.Finish(as(recorderKey), &recorderpb.FinishRequest{
		RecordingId: started.GetId(), Worker: "worker-1", Requeue: true,
		Progress:  &recorderpb.Progress{BytesStored: 100},
		Artifacts: []*recorderpb.Artifact{{Name: "a1.mqa", Kind: "archive", Bytes: 100}},
	}); err != nil {
		t.Fatalf("handing back: %v", err)
	}
	if got := h.get(t, started.GetId()); got.GetState() != adminpb.RecordingState_RECORDING_STATE_RECORDING {
		t.Errorf("a recording handed back is %v, want still RECORDING", got.GetState())
	}

	second := h.claim(t, "worker-2")
	if second.GetAttempt() != 2 {
		t.Errorf("the second worker's attempt is %d, want 2", second.GetAttempt())
	}

	// worker-2 vanishes without a word; its lease runs out.
	h.advance(recorderLease + time.Second)
	third := h.claim(t, "worker-3")
	if third.GetRecordingId() != started.GetId() || third.GetAttempt() != 3 {
		t.Fatalf("after a lapsed lease the claim gave %q attempt %d", third.GetRecordingId(), third.GetAttempt())
	}
	_, err := h.link.Renew(as(recorderKey), &recorderpb.RenewRequest{RecordingId: started.GetId(), Worker: "worker-2"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a worker that lost its lease renewed with %v, want FailedPrecondition", status.Code(err))
	}
	if got := h.get(t, started.GetId()); got.GetBytesStored() != 100 || len(got.GetArtifacts()) != 1 {
		t.Errorf("the first attempt's files are lost: %d bytes, %d artifacts", got.GetBytesStored(), len(got.GetArtifacts()))
	}
}

func TestRecordingsBelongToTheirTenant(t *testing.T) {
	h := newRecordingHarness(t)
	h.createRoom(t, "alice")
	started := h.start(t, roomName)

	_, err := h.recordings.GetRecording(as(otherTenantKey), &adminpb.GetRecordingRequest{Tenant: tenant, RecordingId: started.GetId()})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("another tenant naming ours gave %v, want PermissionDenied", status.Code(err))
	}
	_, err = h.recordings.GetRecording(as(otherTenantKey), &adminpb.GetRecordingRequest{Tenant: "globex", RecordingId: started.GetId()})
	if status.Code(err) != codes.NotFound {
		t.Errorf("another tenant asking for our recording as theirs gave %v, want NotFound", status.Code(err))
	}
	_, err = h.recordings.StopRecording(as(otherTenantKey), &adminpb.StopRecordingRequest{Tenant: "globex", RecordingId: started.GetId()})
	if status.Code(err) != codes.NotFound {
		t.Errorf("another tenant stopping our recording gave %v, want NotFound", status.Code(err))
	}
	listed, err := h.recordings.ListRecordings(as(otherTenantKey), &adminpb.ListRecordingsRequest{Tenant: "globex"})
	if err != nil || len(listed.GetRecordings()) != 0 {
		t.Errorf("another tenant's listing shows %d of ours (%v)", len(listed.GetRecordings()), err)
	}
}

func TestStartingARecordingNeedsWatchdogStanding(t *testing.T) {
	h := newRecordingHarness(t)
	h.createRoom(t, "alice")
	_, err := h.recordings.StartRecording(as(joinKey), &adminpb.StartRecordingRequest{Tenant: tenant, Room: roomName})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("a join-only key started a recording: %v", status.Code(err))
	}
}

func TestRecorderCredentialsReachOnlyTheRecorderLink(t *testing.T) {
	h := newRecordingHarness(t)
	h.createRoom(t, "alice")

	if _, err := h.recordings.ListRecordings(as(recorderKey), &adminpb.ListRecordingsRequest{Tenant: tenant}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a recorder key reached the admin API: %v", status.Code(err))
	}
	if _, err := h.tokens.MintClientToken(as(recorderKey), &adminpb.MintClientTokenRequest{
		Tenant: tenant, Username: "bob", DeviceThumbprint: "abcdefghijklmnopqrst", Room: roomName,
	}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a recorder key minted a client token: %v", status.Code(err))
	}
	for _, key := range []string{ownerKey, relayKey} {
		if _, err := h.link.Claim(as(key), &recorderpb.ClaimRequest{Worker: "w"}); status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s claimed a recording: %v", key, status.Code(err))
		}
	}
}

// The recorder's identity cannot be minted for anybody else, which is what
// makes leaving it off the roster safe.
func TestTheRecorderIdentityIsReserved(t *testing.T) {
	h := newRecordingHarness(t)
	h.createRoom(t, "alice")

	for _, request := range []*adminpb.MintClientTokenRequest{
		{Tenant: tenant, Username: server.RecorderSubject, DeviceThumbprint: "abcdefghijklmnopqrst", Room: roomName},
		{Tenant: tenant, Username: "mallory", DeviceThumbprint: recorderThumbprint, Room: roomName},
	} {
		if _, err := h.tokens.MintClientToken(as(ownerKey), request); status.Code(err) != codes.InvalidArgument {
			t.Errorf("minting as %s on %s gave %v, want InvalidArgument", request.GetUsername(), request.GetDeviceThumbprint(), status.Code(err))
		}
	}
}

func TestTheRecorderIsNotAParticipant(t *testing.T) {
	h := newRecordingHarness(t)
	h.createRoom(t, "alice")
	relay := h.attachRelay(t)
	defer relay.stop()

	relay.openSession(t, "s-recorder", control.Identity{Username: server.RecorderSubject, Device: recorderDevice})
	relay.openSession(t, "s-alice", control.Identity{Username: "alice", Device: "alicephone"})

	list := func() []*adminpb.Participant {
		page, err := h.rooms.ListParticipants(as(ownerKey), &adminpb.ListParticipantsRequest{
			Tenant: tenant, Room: roomName, IncludeBackstage: true, IncludeDeparted: true,
		})
		if err != nil {
			t.Fatalf("listing participants: %v", err)
		}
		return page.GetParticipants()
	}
	waitFor(t, "alice to arrive", func() bool { return len(list()) > 0 })
	for _, participant := range list() {
		if participant.GetIdentity().GetUsername() == server.RecorderSubject {
			t.Error("the recorder appears on the roster")
		}
	}

	// Closing the recorder's session must not fail the relay stream, which
	// would show as alice's departure never being recorded.
	relay.closeSession(t, "s-recorder", relaypb.SessionClosed_CAUSE_CLIENT_CLOSED)
	relay.closeSession(t, "s-alice", relaypb.SessionClosed_CAUSE_CLIENT_CLOSED)
	waitFor(t, "alice to leave", func() bool {
		for _, participant := range list() {
			if participant.GetIdentity().GetUsername() == "alice" {
				return participant.GetPresence() == adminpb.Presence_PRESENCE_LEFT
			}
		}
		return false
	})
}

func TestRecordingIsRefusedWhereItIsNotEnabled(t *testing.T) {
	h := newHarness(t)
	h.createRoom(t, "alice")
	recordings := adminpb.NewRecordingsClient(dial(t, h.listener))
	_, err := recordings.StartRecording(as(ownerKey), &adminpb.StartRecordingRequest{Tenant: tenant, Room: roomName})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("recording on a deployment without it gave %v, want FailedPrecondition", status.Code(err))
	}
}
