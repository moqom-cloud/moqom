package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/moqom-cloud/moqom/go/internal/control"
)

// A backend is not a device, and giving it a device-bound client token would be
// pretending otherwise: it has no secure element, it acts for many users rather
// than one, and its actions have to be attributable to it rather than to whichever
// moderator it last spoke for. So backends authenticate as service accounts, and
// the human they are acting for is a separate, declared field on every call.

// ServiceAccount is a backend's identity and its ceiling.
type ServiceAccount struct {
	Name string

	// Tenant this account may act within. Empty means every tenant, which is a
	// platform-operator account and should be rare: an account scoped to one
	// tenant cannot be tricked into acting on another by a request body naming it.
	Tenant string

	// Grants is the ceiling on what this account can do and, critically, on what
	// it can mint.
	//
	// Without a ceiling, an API key that can create rooms can mint itself an
	// owner token for any of them, and the blast radius of a leaked key is the
	// whole tenant. With one, a key issued for "let users join rooms" cannot
	// produce a moderator.
	Grants control.Grants

	// IsRelay marks an account used by a relay rather than by a customer's
	// backend. Relays reach a different service and must not be able to reach the
	// admin API, so the two are not interchangeable.
	IsRelay bool

	// IsRecorder marks an account used by hosted recorder workers. Like a
	// relay's, it reaches its own service and nothing on the admin API.
	IsRecorder bool

	// RelayID, if set, is the only relay ID this account may attach as,
	// whatever its Hello says. Set for a customer's relay, so one enrolment
	// cannot take over another relay's entry on the link.
	RelayID string

	// Enrollment is the customer relay enrolment this account came from, if
	// any; a stream it holds is cut when the enrolment is revoked.
	Enrollment string
}

// Accounts resolves a credential to an account.
//
// An interface because a real deployment keeps these in Postgres with rotation and
// revocation, and because the tests need to hand over a fixed set without one.
type Accounts interface {
	Lookup(ctx context.Context, credential string) (ServiceAccount, error)
}

// ErrNoSuchAccount is returned for a credential that resolves to nothing.
//
// One error for "no such key" and "wrong key" together, deliberately: telling
// them apart tells an attacker which half of a guess was right.
var ErrNoSuchAccount = errors.New("no such service account")

// ErrNotPermitted and ErrRateLimited are a credential that was proven and then
// refused: a scoped key outside its scope or its allowed addresses, or one over
// its rate. The caller holds the credential, so saying why helps only them,
// and a distinct code stops a client retrying a door that will stay shut.
var (
	ErrNotPermitted = errors.New("credential not permitted for this call")
	ErrRateLimited  = errors.New("credential over its rate limit")
)

// StaticAccounts is an Accounts backed by a fixed map, keyed by the hash of the
// credential.
//
// Hashed rather than plain so a memory dump or a log of this structure does not
// contain anything usable. The credentials themselves exist only in the operator's
// hands and in the request that presents one.
type StaticAccounts map[string]ServiceAccount

// HashCredential returns the key a credential is stored under.
func HashCredential(credential string) string {
	digest := sha256.Sum256([]byte(credential))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// NewStaticAccounts builds a set from plaintext credentials.
func NewStaticAccounts(byCredential map[string]ServiceAccount) StaticAccounts {
	accounts := StaticAccounts{}
	for credential, account := range byCredential {
		accounts[HashCredential(credential)] = account
	}
	return accounts
}

func (a StaticAccounts) Lookup(_ context.Context, credential string) (ServiceAccount, error) {
	hashed := HashCredential(credential)
	// Every candidate is compared, and the comparison is constant-time, so
	// neither the time taken nor the number of comparisons depends on how much of
	// a guessed credential was correct.
	var found ServiceAccount
	matched := 0
	for stored, account := range a {
		if subtle.ConstantTimeCompare([]byte(stored), []byte(hashed)) == 1 {
			found = account
			matched++
		}
	}
	if matched == 0 {
		return ServiceAccount{}, ErrNoSuchAccount
	}
	return found, nil
}

// AnyAccounts tries each source in turn: the operator's static accounts and
// customers' API keys, behind one interceptor.
type AnyAccounts []Accounts

func (a AnyAccounts) Lookup(ctx context.Context, credential string) (ServiceAccount, error) {
	for _, source := range a {
		if source == nil {
			continue
		}
		account, err := source.Lookup(ctx, credential)
		if err == nil {
			return account, nil
		}
		if errors.Is(err, ErrNotPermitted) || errors.Is(err, ErrRateLimited) {
			return ServiceAccount{}, err
		}
	}
	return ServiceAccount{}, ErrNoSuchAccount
}

type principalKey struct{}

// WithPrincipal attaches an account to a context, for tests and for the
// interceptor.
func WithPrincipal(ctx context.Context, account ServiceAccount) context.Context {
	return context.WithValue(ctx, principalKey{}, account)
}

// PrincipalFrom returns the authenticated account.
func PrincipalFrom(ctx context.Context) (ServiceAccount, bool) {
	account, ok := ctx.Value(principalKey{}).(ServiceAccount)
	return account, ok
}

// Authenticator turns credentials on a call into a principal on its context.
type Authenticator struct {
	Accounts Accounts
}

// Unary is a grpc.UnaryServerInterceptor.
func (a Authenticator) Unary(
	ctx context.Context,
	request any,
	_ *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (any, error) {
	authenticated, err := a.authenticate(ctx)
	if err != nil {
		return nil, err
	}
	return handler(authenticated, request)
}

// Stream is a grpc.StreamServerInterceptor.
func (a Authenticator) Stream(
	service any,
	stream grpc.ServerStream,
	_ *grpc.StreamServerInfo,
	handler grpc.StreamHandler,
) error {
	authenticated, err := a.authenticate(stream.Context())
	if err != nil {
		return err
	}
	return handler(service, wrappedStream{ServerStream: stream, ctx: authenticated})
}

type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w wrappedStream) Context() context.Context { return w.ctx }

func (a Authenticator) authenticate(ctx context.Context) (context.Context, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "no credentials")
	}
	values := md.Get("authorization")
	if len(values) == 0 {
		return nil, status.Error(codes.Unauthenticated, "no credentials")
	}
	credential := strings.TrimPrefix(values[0], "Bearer ")
	if credential == values[0] {
		credential = strings.TrimPrefix(values[0], "bearer ")
	}
	if credential == "" {
		return nil, status.Error(codes.Unauthenticated, "empty credential")
	}

	account, err := a.Accounts.Lookup(ctx, credential)
	switch {
	case errors.Is(err, ErrNotPermitted):
		return nil, status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, ErrRateLimited):
		return nil, status.Error(codes.ResourceExhausted, err.Error())
	}
	if err != nil {
		// Deliberately uninformative. Which credential was wrong, and why, is not
		// something an unauthenticated caller gets to learn.
		return nil, status.Error(codes.Unauthenticated, "invalid credentials")
	}
	return WithPrincipal(ctx, account), nil
}

// authorise checks that the caller may act on this tenant at all, and returns the
// account for the checks that follow.
//
// Every service method starts here. The tenant is in the request body, so an
// account scoped to one tenant asking about another is the first thing to refuse --
// before a room is looked up, so the answer does not reveal whether it exists.
func requireTenant(ctx context.Context, tenant string) (ServiceAccount, error) {
	account, ok := PrincipalFrom(ctx)
	if !ok {
		return ServiceAccount{}, status.Error(codes.Unauthenticated, "no principal")
	}
	if account.IsRelay {
		return ServiceAccount{}, status.Error(codes.PermissionDenied, "relay credentials cannot use the admin API")
	}
	if account.IsRecorder {
		return ServiceAccount{}, status.Error(codes.PermissionDenied, "recorder credentials cannot use the admin API")
	}
	if tenant == "" {
		return ServiceAccount{}, status.Error(codes.InvalidArgument, "tenant is required")
	}
	if account.Tenant != "" && account.Tenant != tenant {
		return ServiceAccount{}, status.Errorf(codes.PermissionDenied, "account %s is scoped to tenant %s", account.Name, account.Tenant)
	}
	return account, nil
}

func requireRelay(ctx context.Context) (ServiceAccount, error) {
	account, ok := PrincipalFrom(ctx)
	if !ok {
		return ServiceAccount{}, status.Error(codes.Unauthenticated, "no principal")
	}
	if !account.IsRelay {
		return ServiceAccount{}, status.Error(codes.PermissionDenied, "this stream is for relays")
	}
	return account, nil
}

// statusFor maps a domain error onto a gRPC code.
//
// The mapping is load-bearing rather than cosmetic. A client library retries some
// codes and not others, so returning Unavailable for a permission problem produces
// a moderation tool that hammers a door it will never be let through, and
// returning InvalidArgument for a lost race loses an action a moderator believes
// they took.
func statusFor(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, control.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, control.ErrAlreadyExists):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, control.ErrPermission):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, control.ErrInvalid), errors.Is(err, control.ErrImmutablePolicy):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, control.ErrRoomClosed):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	default:
		// Internal, and the message is kept: this is our own bug, and hiding it
		// from an operator helps nobody. Anything sensitive belongs in the errors
		// above rather than in a default case.
		return status.Error(codes.Internal, fmt.Sprintf("internal error: %v", err))
	}
}
