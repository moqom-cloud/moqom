package moqom

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The errors a caller has to distinguish.
//
// Sentinels rather than only status codes, because a caller should not have to import
// gRPC to tell "no such room" from "not allowed". Compare with errors.Is:
//
//	if _, err := room.Kick(ctx, request); errors.Is(err, moqom.ErrDenied) {
//		// the moderator lacks standing over this target
//	}
var (
	// ErrNotFound is a room, participant, or watchdog that does not exist. Also
	// what a caller sees when the thing exists in another tenant, deliberately:
	// existence is not something a stranger gets to probe.
	ErrNotFound = errors.New("not found")

	// ErrDenied is a permission problem: the account's ceiling, the actor's
	// capability set, or standing over the target. Not retryable, and the message
	// says which of the three it was.
	ErrDenied = errors.New("permission denied")

	// ErrUnauthenticated is a missing, malformed, or unknown credential.
	ErrUnauthenticated = errors.New("unauthenticated")

	// ErrInvalid is a malformed request -- an unknown capability, a bad CIDR, a
	// room name over the length limit. Retrying will not fix it.
	ErrInvalid = errors.New("invalid argument")

	// ErrExists is a room that is already there.
	ErrExists = errors.New("already exists")

	// ErrClosed is an action on a room whose lifecycle has ended, including
	// minting a token into one.
	ErrClosed = errors.New("room is closed")

	// ErrUnavailable is a transient failure that survived the SDK's own retries.
	// Worth retrying later with the same idempotency key.
	ErrUnavailable = errors.New("unavailable")

	// ErrInternal is our bug. The message is kept rather than swallowed, because
	// an operator reading it is more useful than a tidy error.
	ErrInternal = errors.New("internal error")
)

// Error is what every call returns on failure.
//
// It names the operation as well as the cause, because "permission denied" without a
// verb is the kind of log line that costs an hour.
type Error struct {
	// Op is the SDK method, such as "Room.Kick".
	Op string

	// Code is the gRPC status code, for callers that want the original.
	Code codes.Code

	// Message is the server's explanation.
	Message string

	// kind is the sentinel this unwraps to.
	kind error

	// cause is a second thing this unwraps to, where a failure is honestly two
	// things at once: a deadline is both ErrUnavailable and context.DeadlineExceeded,
	// and a caller should be able to test for either.
	cause error
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("moqom: %s: %v", e.Op, e.kind)
	}
	return fmt.Sprintf("moqom: %s: %v: %s", e.Op, e.kind, e.Message)
}

// Unwrap returns what this error also is, so errors.Is works against the list above.
func (e *Error) Unwrap() []error {
	if e.cause == nil {
		return []error{e.kind}
	}
	return []error{e.kind, e.cause}
}

// Retryable reports whether trying again could succeed.
//
// The SDK already retries these within a call; this is for a caller deciding whether
// to requeue. Reuse the idempotency key when you do, or a retry of something that
// actually landed will act twice.
func (e *Error) Retryable() bool {
	return e.Code == codes.Unavailable || e.Code == codes.DeadlineExceeded
}

func wrap(operation string, err error) error {
	if err == nil {
		return nil
	}

	statusValue, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("moqom: %s: %w", operation, err)
	}

	// Cancellation is the caller's own doing, and dressing it up as an SDK failure
	// makes a clean shutdown look like an outage. It arrives as a status code rather
	// than as context.Canceled, which is worth catching here rather than in every
	// caller's error handling.
	if statusValue.Code() == codes.Canceled || errors.Is(err, context.Canceled) {
		return fmt.Errorf("moqom: %s: %w", operation, context.Canceled)
	}

	kind, cause := ErrInternal, error(nil)
	switch statusValue.Code() {
	case codes.NotFound:
		kind = ErrNotFound
	case codes.PermissionDenied:
		kind = ErrDenied
	case codes.Unauthenticated:
		kind = ErrUnauthenticated
	case codes.InvalidArgument:
		kind = ErrInvalid
	case codes.AlreadyExists:
		kind = ErrExists
	case codes.FailedPrecondition:
		kind = ErrClosed
	case codes.Unavailable:
		kind = ErrUnavailable
	case codes.DeadlineExceeded:
		kind, cause = ErrUnavailable, context.DeadlineExceeded
	}

	return &Error{
		Op:      operation,
		Code:    statusValue.Code(),
		Message: statusValue.Message(),
		kind:    kind,
		cause:   cause,
	}
}

// retryable decides whether the SDK retries within a call.
//
// Only codes that cannot have applied a change, plus the two that mean "we do not
// know whether it applied" -- which are safe precisely because every mutating call
// carries an idempotency key.
func retryable(err error) bool {
	statusValue, ok := status.FromError(err)
	if !ok {
		return false
	}
	switch statusValue.Code() {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Aborted:
		return true
	default:
		return false
	}
}
