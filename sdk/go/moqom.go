// Package moqom is the server-side SDK for MOQOM.
//
// It is for a backend, not a participant. A backend mints the tokens its users join
// with, creates and closes rooms, moderates them, and follows what happens in them.
// It does not carry media: nothing here speaks MOQT, and a Go process that wants to
// receive frames wants the Rust SDK instead.
//
// The split is deliberate. Media needs a QUIC stack, a codec, and a scheduler on the
// hot path; administration needs none of those, and making every backend integration
// link a media stack to change somebody's grants would be a poor trade. So this is a
// gRPC client over the control plane's admin API, and it is small.
//
// # Getting started
//
//	client, err := moqom.Dial("api.moqom.cloud:443",
//		moqom.WithCredential(os.Getenv("MOQOM_SERVICE_KEY")),
//		moqom.WithTenant("acme"))
//	if err != nil {
//		return err
//	}
//	defer client.Close()
//
//	// The call a backend makes most: hand a signed-in user a token to join with.
//	token, err := client.MintToken(ctx, moqom.TokenRequest{
//		Username:         user.Name,
//		DeviceThumbprint: request.Thumbprint,
//		Room:             "live/42",
//		Preset:           moqom.PresetNone,
//		Publish:          moqom.PublishAudio | moqom.PublishVideo,
//	})
//
//	// Moderation, on behalf of the human who pressed the button.
//	room := client.Room("live/42")
//	_, err = room.Kick(ctx, moqom.KickRequest{
//		Target: moqom.User("carol"),
//		Code:   moqom.CodeHarassment,
//		Actor:  moqom.Actor{OnBehalfOf: "jane", Reason: "repeated slurs in chat"},
//	})
//
// # Identity
//
// There are no numeric user IDs. A username is a string the customer already has, and
// a device is derived from the device key's thumbprint, so a device ban and the
// thumbprint a backend passed to MintToken are the same fact. This is what makes a
// migration from a platform with its own UID space a matter of pointing at a new
// server rather than building a mapping table.
//
// # Retries
//
// Every mutating call carries an idempotency key, generated here when the caller does
// not supply one, and retried calls reuse it. A queue that redelivers "ban for 24
// hours" therefore does not extend the ban. Supply your own key in Actor when the
// retry might come from a different process than the one that made the first attempt,
// since a key generated here cannot outlive it.
package moqom

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/moqom-cloud/moqom-go/admin/adminpb"
)

// Client is a connection to a MOQOM control plane.
//
// Safe for concurrent use, and meant to be long-lived: it holds one HTTP/2 connection
// that every call shares, so constructing one per request throws away the handshake
// that makes the calls cheap.
type Client struct {
	conn  grpc.ClientConnInterface
	close func() error

	tenant string
	actor  Actor
	retry  retryPolicy
	keys   func() string

	rooms      adminpb.RoomsClient
	moderation adminpb.ModerationClient
	tokens     adminpb.TokensClient
	events     adminpb.EventsClient
}

// Actor is who an action is attributed to, and why.
//
// The service account is established by the credentials on the connection and cannot
// be set here -- a caller that could name its own actor could forge an audit trail.
// What this adds is the human the backend is acting for, which the server has no way
// to know: a kick from a customer's own moderation console is attributable to that
// moderator rather than to an API key.
type Actor struct {
	// The moderator this action is on behalf of. Empty for something the backend
	// decided on its own, such as an automated safety verdict.
	OnBehalfOf string

	// Free text, stored on the moderation entry and shown in appeals. Worth
	// insisting on internally even where the server does not require it.
	Reason string

	// Deduplication key. Left empty, one is generated per call, which covers a
	// retry inside this process but not a redelivery from a queue -- for that,
	// derive a key from the message and set it here.
	IdempotencyKey string
}

// merge fills the gaps in a per-call actor from the client's default.
func (a Actor) merge(fallback Actor) Actor {
	if a.OnBehalfOf == "" {
		a.OnBehalfOf = fallback.OnBehalfOf
	}
	if a.Reason == "" {
		a.Reason = fallback.Reason
	}
	return a
}

// Option configures a Client.
type Option func(*settings) error

type settings struct {
	tenant      string
	credential  string
	actor       Actor
	conn        grpc.ClientConnInterface
	closeConn   bool
	dialOptions []grpc.DialOption
	transport   credentials.TransportCredentials
	insecure    bool
	retry       retryPolicy
	keys        func() string
}

// WithTenant sets which tenant this client acts on.
//
// Required. Every call names its tenant on the wire, and a client that guessed would
// be a client whose calls fail one deployment later.
func WithTenant(tenant string) Option {
	return func(s *settings) error {
		if tenant == "" {
			return errors.New("moqom: the tenant cannot be empty")
		}
		s.tenant = tenant
		return nil
	}
}

// WithCredential sets the service account credential.
//
// It is sent as a bearer token and, by default, only over TLS: the credential's own
// transport requirement makes gRPC refuse to send it in the clear rather than leaving
// that to a reviewer to notice.
func WithCredential(credential string) Option {
	return func(s *settings) error {
		if credential == "" {
			return errors.New("moqom: the credential cannot be empty")
		}
		s.credential = credential
		return nil
	}
}

// WithActor sets the default attribution for every call.
//
// Useful for a backend that is only ever one thing -- a safety pipeline setting
// Reason once rather than at every call site. A per-call Actor still wins.
func WithActor(actor Actor) Option {
	return func(s *settings) error {
		s.actor = actor
		return nil
	}
}

// WithTLS replaces the transport credentials.
//
// The default is the system roots with TLS 1.3 as the floor. Use this for a private
// certificate authority or for pinning.
func WithTLS(transport credentials.TransportCredentials) Option {
	return func(s *settings) error {
		if transport == nil {
			return errors.New("moqom: WithTLS needs credentials; use WithoutTransportSecurity to disable TLS")
		}
		s.transport = transport
		return nil
	}
}

// WithoutTransportSecurity disables TLS.
//
// For a test, a sidecar on loopback, or a mesh that terminates TLS elsewhere. It also
// waives the credential's refusal to travel in the clear, which is the part worth
// pausing over: on any network a third party can see, this hands out a bearer token
// that grants everything the service account can do.
func WithoutTransportSecurity() Option {
	return func(s *settings) error {
		s.insecure = true
		return nil
	}
}

// WithDialOptions passes options through to gRPC.
//
// An escape hatch for load balancing, interceptors, and keepalive tuning. Options
// given here are applied after the SDK's own, so they win.
func WithDialOptions(options ...grpc.DialOption) Option {
	return func(s *settings) error {
		s.dialOptions = append(s.dialOptions, options...)
		return nil
	}
}

// WithConnection uses a connection the caller already has.
//
// For a process that multiplexes several APIs over one connection, or a test using
// bufconn. The connection is not closed by Close, since this client did not open it.
func WithConnection(conn grpc.ClientConnInterface) Option {
	return func(s *settings) error {
		if conn == nil {
			return errors.New("moqom: WithConnection needs a connection")
		}
		s.conn = conn
		return nil
	}
}

// WithRetry configures how transient failures are retried.
//
// Attempts of one disables retrying. Only codes that cannot have applied a change are
// retried, and every attempt reuses the idempotency key, so a retry of a call that
// did land returns the original result rather than repeating it.
func WithRetry(attempts int, base time.Duration) Option {
	return func(s *settings) error {
		if attempts < 1 {
			return errors.New("moqom: retry needs at least one attempt")
		}
		s.retry = retryPolicy{attempts: attempts, base: base}
		return nil
	}
}

// Dial builds a client.
//
// It does not connect: gRPC dials lazily, so a bad address surfaces at the first call
// rather than here. That is worth knowing when writing a health check, because
// constructing a client is not one.
func Dial(target string, options ...Option) (*Client, error) {
	if target == "" {
		return nil, errors.New("moqom: the target cannot be empty")
	}

	settings := settings{
		retry: retryPolicy{attempts: 3, base: 100 * time.Millisecond},
		keys:  randomKey,
	}
	for _, option := range options {
		if err := option(&settings); err != nil {
			return nil, err
		}
	}
	if settings.tenant == "" {
		return nil, errors.New("moqom: WithTenant is required")
	}

	conn := settings.conn
	closer := func() error { return nil }
	if conn == nil {
		if settings.credential == "" {
			return nil, errors.New("moqom: WithCredential is required")
		}

		transport := settings.transport
		switch {
		case settings.insecure:
			transport = insecure.NewCredentials()
		case transport == nil:
			transport = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13})
		}

		dialOptions := []grpc.DialOption{
			grpc.WithTransportCredentials(transport),
			grpc.WithPerRPCCredentials(bearer{
				token: settings.credential,
				// Waived only when the caller has explicitly turned TLS off, so
				// the default cannot leak a credential by omission.
				requireSecurity: !settings.insecure,
			}),
		}
		dialOptions = append(dialOptions, settings.dialOptions...)

		dialled, err := grpc.NewClient(target, dialOptions...)
		if err != nil {
			return nil, fmt.Errorf("moqom: dialling %s: %w", target, err)
		}
		conn, closer = dialled, dialled.Close
	}

	return &Client{
		conn:       conn,
		close:      closer,
		tenant:     settings.tenant,
		actor:      settings.actor,
		retry:      settings.retry,
		keys:       settings.keys,
		rooms:      adminpb.NewRoomsClient(conn),
		moderation: adminpb.NewModerationClient(conn),
		tokens:     adminpb.NewTokensClient(conn),
		events:     adminpb.NewEventsClient(conn),
	}, nil
}

// Tenant is the tenant this client acts on.
func (c *Client) Tenant() string { return c.tenant }

// Close releases the connection, unless it came from WithConnection.
func (c *Client) Close() error { return c.close() }

// Room returns a handle for one room.
//
// No I/O: it is a name plus this client, and every method on it is a call. Get
// reads the room if that is what you wanted.
func (c *Client) Room(name string) *RoomHandle {
	return &RoomHandle{client: c, name: name}
}

// RoomHandle addresses one room.
type RoomHandle struct {
	client *Client
	name   string
}

// Name is the room's identifier within the tenant.
func (r *RoomHandle) Name() string { return r.name }

// bearer sends the service account credential as an Authorization header.
type bearer struct {
	token           string
	requireSecurity bool
}

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + b.token}, nil
}

func (b bearer) RequireTransportSecurity() bool { return b.requireSecurity }

// actorFor builds the wire actor, generating an idempotency key when needed.
//
// One key per logical call rather than per attempt: that is the whole point, and
// generating it inside the retry loop would defeat it silently.
func (c *Client) actorFor(actor Actor) *adminpb.ActorContext {
	merged := actor.merge(c.actor)
	key := merged.IdempotencyKey
	if key == "" {
		key = c.keys()
	}
	return &adminpb.ActorContext{
		OnBehalfOf:     merged.OnBehalfOf,
		Reason:         merged.Reason,
		IdempotencyKey: key,
	}
}

func randomKey() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// Only reachable if the system entropy source is broken, at which point a
		// key colliding is the least of the caller's problems. A timestamp keeps
		// the call from failing outright.
		return fmt.Sprintf("t-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(raw[:])
}

type retryPolicy struct {
	attempts int
	base     time.Duration
}

// call runs an RPC, retrying the failures that cannot have applied a change.
//
// The unit of retry is the whole call including its idempotency key, so a retry after
// a timeout on a request the server did process returns that first result rather than
// acting twice.
func call[T any](ctx context.Context, c *Client, operation string, do func(context.Context) (T, error)) (T, error) {
	var zero T
	policy := c.retry
	if policy.attempts < 1 {
		policy.attempts = 1
	}

	var lastErr error
	for attempt := range policy.attempts {
		if attempt > 0 {
			delay := policy.base * time.Duration(math.Pow(2, float64(attempt-1)))
			if !sleep(ctx, delay) {
				return zero, wrap(operation, ctx.Err())
			}
		}

		result, err := do(ctx)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !retryable(err) {
			return zero, wrap(operation, err)
		}
	}
	return zero, wrap(operation, lastErr)
}

func sleep(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
