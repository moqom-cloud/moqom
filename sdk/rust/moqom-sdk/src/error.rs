//! What a call can fail with.

use tonic::Code;

/// The result of every call in this crate.
pub type Result<T> = std::result::Result<T, Error>;

/// What went wrong.
///
/// Variants rather than a code, because a caller should be able to match on the
/// distinction that matters without knowing gRPC's numbering:
///
/// ```no_run
/// # async fn example(room: &moqom_sdk::Room<'_>, request: moqom_sdk::KickRequest) {
/// match room.kick(request).await {
///     Ok(entry) if !entry.enforced => { /* recorded, enforcement catching up */ }
///     Ok(_) => {}
///     Err(moqom_sdk::Error::Denied { .. }) => { /* no standing over this target */ }
///     Err(other) => eprintln!("{other}"),
/// }
/// # }
/// ```
#[derive(Debug, thiserror::Error)]
#[non_exhaustive]
pub enum Error {
    /// A room, participant, or watchdog that does not exist.
    ///
    /// Also what a caller sees for something that exists in another tenant,
    /// deliberately: existence is not something a stranger gets to probe.
    #[error("{operation}: not found: {message}")]
    NotFound {
        /// The SDK method that failed.
        operation: &'static str,
        /// The server's explanation.
        message: String,
    },

    /// A permission problem: the account's ceiling, the actor's capability set, or
    /// standing over the target. The message says which.
    #[error("{operation}: permission denied: {message}")]
    Denied {
        /// The SDK method that failed.
        operation: &'static str,
        /// The server's explanation.
        message: String,
    },

    /// A missing, malformed, or unknown credential.
    #[error("{operation}: unauthenticated: {message}")]
    Unauthenticated {
        /// The SDK method that failed.
        operation: &'static str,
        /// The server's explanation.
        message: String,
    },

    /// A malformed request. Retrying will not fix it.
    #[error("{operation}: invalid: {message}")]
    Invalid {
        /// The SDK method that failed.
        operation: &'static str,
        /// The server's explanation.
        message: String,
    },

    /// A room that already exists.
    #[error("{operation}: already exists: {message}")]
    Exists {
        /// The SDK method that failed.
        operation: &'static str,
        /// The server's explanation.
        message: String,
    },

    /// An action on a room whose lifecycle has ended, including minting a token into
    /// one.
    #[error("{operation}: the room is closed: {message}")]
    Closed {
        /// The SDK method that failed.
        operation: &'static str,
        /// The server's explanation.
        message: String,
    },

    /// A transient failure that survived this crate's own retries.
    ///
    /// Worth retrying later, with the same idempotency key: reusing it is what makes
    /// the retry safe if the first attempt actually landed.
    #[error("{operation}: unavailable: {message}")]
    Unavailable {
        /// The SDK method that failed.
        operation: &'static str,
        /// The server's explanation.
        message: String,
    },

    /// The call was cancelled by the caller. Not a failure of the service, and
    /// treating it as one makes a clean shutdown look like an outage.
    #[error("{operation}: cancelled")]
    Cancelled {
        /// The SDK method that failed.
        operation: &'static str,
    },

    /// Our bug. The message is kept rather than swallowed: an operator reading it is
    /// more useful than a tidy error.
    #[error("{operation}: internal error: {message}")]
    Internal {
        /// The SDK method that failed.
        operation: &'static str,
        /// The server's explanation.
        message: String,
    },

    /// A request this crate refused to send.
    ///
    /// A ban with two subjects is ambiguous about which was meant, so it is refused
    /// here: the round trip would only report the same thing later.
    #[error("{0}")]
    Request(String),

    /// The connection could not be established or configured.
    #[error("connecting: {0}")]
    Connect(#[from] tonic::transport::Error),
}

impl Error {
    /// Whether trying again could succeed.
    ///
    /// This crate already retries these within a call; this is for a caller deciding
    /// whether to requeue. Reuse the idempotency key when you do.
    #[must_use]
    pub fn retryable(&self) -> bool {
        matches!(self, Error::Unavailable { .. })
    }

    /// The operation that failed, for a log line that says what it was.
    #[must_use]
    pub fn operation(&self) -> &str {
        match self {
            Error::NotFound { operation, .. }
            | Error::Denied { operation, .. }
            | Error::Unauthenticated { operation, .. }
            | Error::Invalid { operation, .. }
            | Error::Exists { operation, .. }
            | Error::Closed { operation, .. }
            | Error::Unavailable { operation, .. }
            | Error::Cancelled { operation }
            | Error::Internal { operation, .. } => operation,
            Error::Request(_) => "request",
            Error::Connect(_) => "connect",
        }
    }

    pub(crate) fn from_status(operation: &'static str, status: &tonic::Status) -> Self {
        let message = status.message().to_owned();
        match status.code() {
            Code::NotFound => Error::NotFound { operation, message },
            Code::PermissionDenied => Error::Denied { operation, message },
            Code::Unauthenticated => Error::Unauthenticated { operation, message },
            Code::InvalidArgument => Error::Invalid { operation, message },
            Code::AlreadyExists => Error::Exists { operation, message },
            Code::FailedPrecondition => Error::Closed { operation, message },
            Code::Unavailable | Code::DeadlineExceeded | Code::Aborted => {
                Error::Unavailable { operation, message }
            }
            Code::Cancelled => Error::Cancelled { operation },
            _ => Error::Internal { operation, message },
        }
    }

    pub(crate) fn request(message: impl Into<String>) -> Self {
        Error::Request(message.into())
    }
}

/// Whether a failure can be retried within a call.
///
/// Only codes that cannot have applied a change, plus the two meaning "we do not know
/// whether it applied" -- which are safe precisely because every mutating call carries
/// an idempotency key.
pub(crate) fn retryable(status: &tonic::Status) -> bool {
    matches!(
        status.code(),
        Code::Unavailable | Code::DeadlineExceeded | Code::Aborted
    )
}
