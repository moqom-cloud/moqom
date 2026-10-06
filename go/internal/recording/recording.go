// Package recording is the control plane's record of hosted recordings.
//
// A recording is work for a pool of recorder workers, handed out by lease. The
// control plane never runs a recorder itself: it accepts the request, attaches
// the watchdog that tells the room, and waits for a worker to claim the
// recording. The worker renews its lease while it records and reports what it
// stored when it finishes. A worker that stops renewing loses the lease, and
// the next claim resumes the recording under a new attempt number, beside
// whatever the first attempt managed to store.
//
// Kept apart from internal/store for the reason billing and key delivery are:
// it has its own lifecycle and its own tables, and nothing in the room store's
// transactions needs to include it.
package recording

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moqom-cloud/moqom/go/internal/control"
)

// State is where a recording is in its life.
type State string

const (
	StatePending   State = "pending"
	StateRecording State = "recording"
	StateStopping  State = "stopping"
	StateCompleted State = "completed"
	StateFailed    State = "failed"
)

// Terminal reports whether nothing further will happen to a recording in this
// state.
func (s State) Terminal() bool { return s == StateCompleted || s == StateFailed }

// Format is what the caller asked to be written. The archive is always
// written; the question is only whether CMAF is wanted where possible.
type Format string

const (
	FormatArchive        Format = "archive"
	FormatArchiveAndCMAF Format = "archive_and_cmaf"
)

// Remux reports whether CMAF is wanted. Anything but an explicit archive-only
// request wants it, because that is the default the API documents.
func (f Format) Remux() bool { return f != FormatArchive }

// Artifact is one finished file.
type Artifact struct {
	// Name is relative to the recording's prefix.
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Track string `json:"track,omitempty"`
	Bytes uint64 `json:"bytes"`
	// CRC32C is base64, as GCS reports it, so a reader can check a download
	// against what the recorder wrote.
	CRC32C  string `json:"crc32c,omitempty"`
	Attempt uint32 `json:"attempt"`
}

// Progress is what a worker reports.
type Progress struct {
	Objects       uint64 `json:"objects"`
	BytesReceived uint64 `json:"bytes_received"`
	BytesStored   uint64 `json:"bytes_stored"`
	Tracks        uint32 `json:"tracks"`
	Reconnects    uint32 `json:"reconnects"`
}

// Add returns the sum, for totals across attempts.
func (p Progress) Add(other Progress) Progress {
	return Progress{
		Objects:       p.Objects + other.Objects,
		BytesReceived: p.BytesReceived + other.BytesReceived,
		BytesStored:   p.BytesStored + other.BytesStored,
		Tracks:        max(p.Tracks, other.Tracks),
		Reconnects:    p.Reconnects + other.Reconnects,
	}
}

// Recording is one request to record a room.
type Recording struct {
	ID     string `json:"id"`
	Tenant string `json:"tenant"`
	Room   string `json:"room"`
	State  State  `json:"state"`
	Format Format `json:"format,omitempty"`
	// MaxDuration, if set, stops the recording that long after it started.
	MaxDuration time.Duration `json:"max_duration,omitempty"`
	// Automatic is a recording the room's policy started rather than a call.
	Automatic bool `json:"automatic,omitempty"`
	// Encrypted is whether the room required end-to-end encryption when the
	// recording was last claimed, in which case everything stored is ciphertext.
	Encrypted bool `json:"encrypted"`

	// Bucket and Prefix are where artifacts go. The prefix begins with the
	// tenant, so no recording's files can land among another tenant's.
	Bucket string `json:"bucket"`
	Prefix string `json:"prefix"`

	// WatchdogID is the recording watchdog that discloses this to the room.
	WatchdogID string `json:"watchdog_id,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	StartedAt time.Time `json:"started_at,omitzero"`
	EndedAt   time.Time `json:"ended_at,omitzero"`

	// StopRequested is set by a stop, a room closing, a policy change, or the
	// deadline, and read by the worker on its next renewal.
	StopRequested bool   `json:"stop_requested,omitempty"`
	Error         string `json:"error,omitempty"`

	// The lease. Worker is empty when nobody holds it.
	Worker     string    `json:"worker,omitempty"`
	LeaseUntil time.Time `json:"lease_until,omitzero"`
	// Attempt counts claims, from 1.
	Attempt uint32 `json:"attempt"`
	// CredentialExpires is when the relay token the worker holds runs out, so
	// a renewal knows whether to mint another.
	CredentialExpires time.Time `json:"credential_expires,omitzero"`

	// Current is the attempt in progress, as last reported; Stored is every
	// attempt before it.
	Current Progress `json:"current"`
	Stored  Progress `json:"stored"`

	Artifacts []Artifact `json:"artifacts,omitempty"`
	// Manifests are the manifest names, one per attempt that finished.
	Manifests []string `json:"manifests,omitempty"`
}

// Totals is every attempt's progress together.
func (r Recording) Totals() Progress { return r.Stored.Add(r.Current) }

// Deadline is when the recording stops of its own accord, or zero for never.
func (r Recording) Deadline() time.Time {
	if r.MaxDuration <= 0 || r.StartedAt.IsZero() {
		return time.Time{}
	}
	return r.StartedAt.Add(r.MaxDuration)
}

// Claimable reports whether a worker may take this recording now.
func (r Recording) Claimable(now time.Time) bool {
	return !r.State.Terminal() && (r.Worker == "" || !now.Before(r.LeaseUntil))
}

// ErrActive refuses a second recording of a room that is already being
// recorded. One at a time per room, because two would be two viewers billed
// for the same media and two sets of files nobody asked for.
var ErrActive = fmt.Errorf("%w: the room is already being recorded", control.ErrAlreadyExists)

// ErrLeaseLost is a worker reporting on a recording it no longer holds.
var ErrLeaseLost = errors.New("the lease on this recording is held by another worker, or by none")

// Query selects recordings for a listing.
type Query struct {
	Tenant string
	// Room empty for the whole tenant.
	Room      string
	PageSize  uint32
	PageToken string
}

// Page is one page of a listing, newest first.
type Page struct {
	Recordings    []Recording
	NextPageToken string
}

// Store keeps recordings.
type Store interface {
	// Create stores a new recording, refusing with ErrActive if the room
	// already has one that has not ended.
	Create(ctx context.Context, recording Recording) (Recording, error)
	// Get returns a recording if it belongs to tenant. A recording that
	// exists under another tenant is reported as not found.
	Get(ctx context.Context, tenant, id string) (Recording, error)
	List(ctx context.Context, query Query) (Page, error)
	// Active returns a room's recordings that have not ended.
	Active(ctx context.Context, tenant, room string) ([]Recording, error)
	// Update applies change atomically. An error from change aborts it.
	Update(ctx context.Context, id string, change func(*Recording) error) (Recording, error)
	// Claim takes the oldest claimable recording for worker, bumping its
	// attempt and folding the previous attempt's last report into Stored. ok
	// is false when nothing is waiting.
	Claim(ctx context.Context, worker string, now time.Time, lease time.Duration) (claimed Recording, ok bool, err error)
}

// Claimed is what Claim does to a recording, shared by both stores so they
// cannot disagree about it.
func Claimed(r *Recording, worker string, now time.Time, lease time.Duration) {
	r.Stored = r.Stored.Add(r.Current)
	r.Current = Progress{}
	r.Worker = worker
	r.LeaseUntil = now.Add(lease)
	r.Attempt++
	r.CredentialExpires = time.Time{}
	if r.State == StatePending {
		r.State = StateRecording
	}
	if r.StartedAt.IsZero() {
		r.StartedAt = now
	}
}

// Size is the page size a query actually gets.
func (q Query) Size() int {
	switch {
	case q.PageSize == 0:
		return 100
	case q.PageSize > 1000:
		return 1000
	default:
		return int(q.PageSize)
	}
}

// Cursor encodes the position after a recording, newest first.
func Cursor(r Recording) string {
	raw := strconv.FormatInt(r.CreatedAt.UnixNano(), 10) + ":" + r.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// ParseCursor decodes a page token.
func ParseCursor(token string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: page token", control.ErrInvalid)
	}
	nanos, id, ok := strings.Cut(string(raw), ":")
	if !ok {
		return time.Time{}, "", fmt.Errorf("%w: page token", control.ErrInvalid)
	}
	at, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: page token", control.ErrInvalid)
	}
	return time.Unix(0, at), id, nil
}

// newer reports whether a sorts before b in a listing: newest first, ID as
// the tie-break so the order is total.
func newer(a, b Recording) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	return a.ID > b.ID
}

// Memory is a Store in memory, for tests and a laptop.
type Memory struct {
	mu         sync.Mutex
	recordings map[string]Recording
}

var _ Store = (*Memory)(nil)

// NewMemory returns an empty store.
func NewMemory() *Memory { return &Memory{recordings: map[string]Recording{}} }

func clone(r Recording) Recording {
	r.Artifacts = slices.Clone(r.Artifacts)
	r.Manifests = slices.Clone(r.Manifests)
	return r
}

func (m *Memory) Create(_ context.Context, recording Recording) (Recording, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.recordings[recording.ID]; exists {
		return Recording{}, fmt.Errorf("%w: recording %s", control.ErrAlreadyExists, recording.ID)
	}
	for _, existing := range m.recordings {
		if existing.Tenant == recording.Tenant && existing.Room == recording.Room && !existing.State.Terminal() {
			return Recording{}, ErrActive
		}
	}
	m.recordings[recording.ID] = clone(recording)
	return clone(recording), nil
}

func (m *Memory) Get(_ context.Context, tenant, id string) (Recording, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	found, ok := m.recordings[id]
	if !ok || found.Tenant != tenant {
		return Recording{}, fmt.Errorf("%w: recording %s", control.ErrNotFound, id)
	}
	return clone(found), nil
}

func (m *Memory) List(_ context.Context, query Query) (Page, error) {
	var after *Recording
	if query.PageToken != "" {
		at, id, err := ParseCursor(query.PageToken)
		if err != nil {
			return Page{}, err
		}
		after = &Recording{ID: id, CreatedAt: at}
	}
	m.mu.Lock()
	var matching []Recording
	for _, r := range m.recordings {
		if r.Tenant != query.Tenant || (query.Room != "" && r.Room != query.Room) {
			continue
		}
		if after != nil && !newer(*after, r) {
			continue
		}
		matching = append(matching, clone(r))
	}
	m.mu.Unlock()
	slices.SortFunc(matching, func(a, b Recording) int {
		if newer(a, b) {
			return -1
		}
		return 1
	})
	page := Page{}
	if len(matching) > query.Size() {
		matching = matching[:query.Size()]
		page.NextPageToken = Cursor(matching[len(matching)-1])
	}
	page.Recordings = matching
	return page, nil
}

func (m *Memory) Active(_ context.Context, tenant, room string) ([]Recording, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var active []Recording
	for _, r := range m.recordings {
		if r.Tenant == tenant && r.Room == room && !r.State.Terminal() {
			active = append(active, clone(r))
		}
	}
	return active, nil
}

func (m *Memory) Update(_ context.Context, id string, change func(*Recording) error) (Recording, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	found, ok := m.recordings[id]
	if !ok {
		return Recording{}, fmt.Errorf("%w: recording %s", control.ErrNotFound, id)
	}
	next := clone(found)
	if err := change(&next); err != nil {
		return Recording{}, err
	}
	m.recordings[id] = clone(next)
	return next, nil
}

func (m *Memory) Claim(_ context.Context, worker string, now time.Time, lease time.Duration) (Recording, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var oldest *Recording
	for _, r := range m.recordings {
		if !r.Claimable(now) {
			continue
		}
		if oldest == nil || r.CreatedAt.Before(oldest.CreatedAt) ||
			(r.CreatedAt.Equal(oldest.CreatedAt) && r.ID < oldest.ID) {
			candidate := r
			oldest = &candidate
		}
	}
	if oldest == nil {
		return Recording{}, false, nil
	}
	next := clone(*oldest)
	Claimed(&next, worker, now, lease)
	m.recordings[next.ID] = clone(next)
	return next, true, nil
}

// StorageUsage is bytes an attempt left in storage.
type StorageUsage struct {
	Tenant    string
	Room      string
	Recording string
	Attempt   uint32
	Bytes     uint64
	Artifacts int
	At        time.Time
}

// StorageMeter is told what each attempt stored, so storage can be billed at
// cost alongside the recorder's viewing.
//
// A hook rather than a call into billing because billing has no storage
// product yet: what it needs is byte-days, and those come from this plus the
// deletions a retention policy will make. Until that exists LogMeter records
// the facts so they can be billed from the log.
type StorageMeter interface {
	Stored(ctx context.Context, usage StorageUsage) error
}

// LogMeter writes each report to the structured log.
type LogMeter struct{}

func (LogMeter) Stored(ctx context.Context, usage StorageUsage) error {
	slog.InfoContext(ctx, "recording storage",
		"event", "recording_storage_metered",
		"tenant", usage.Tenant, "room", usage.Room, "recording", usage.Recording,
		"attempt", usage.Attempt, "bytes", usage.Bytes, "artifacts", usage.Artifacts)
	return nil
}

// URLSigner makes a time-limited download link for one stored object.
type URLSigner interface {
	SignedURL(ctx context.Context, bucket, object string, lifetime time.Duration, now time.Time) (string, error)
}
