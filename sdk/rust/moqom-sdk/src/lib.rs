//! Server-side SDK for MOQOM.
//!
//! For a backend, not a participant. A backend mints the tokens its users join with,
//! creates and closes rooms, moderates them, and follows what happens in them. Nothing
//! here speaks MOQT: this is a gRPC client over the control plane's admin API, and a
//! Rust process that wants to *receive frames* -- a recorder, a safety classifier -- is
//! a MOQT subscriber and is not this crate's job.
//!
//! ```no_run
//! use moqom_sdk::{Actor, Client, Code, KickRequest, Preset, Publish, TokenRequest, target};
//!
//! # async fn example(credential: String, thumbprint: String) -> Result<(), moqom_sdk::Error> {
//! let client = Client::builder("https://api.moqom.cloud")
//!     .tenant("acme")
//!     .credential(credential)
//!     .connect()
//!     .await?;
//!
//! // The call a backend makes most: hand a signed-in user a token to join with.
//! let token = client
//!     .mint_token(TokenRequest {
//!         username: "bob".into(),
//!         device_thumbprint: thumbprint,
//!         room: "live/42".into(),
//!         publish: Publish::CAMERA,
//!         preset: Some(Preset::None),
//!         ..Default::default()
//!     })
//!     .await?;
//!
//! // Moderation, attributed to the human who pressed the button.
//! let room = client.room("live/42");
//! room.kick(KickRequest {
//!     target: target("carol"),
//!     code: Code::Harassment,
//!     actor: Actor::on_behalf_of("jane").because("repeated slurs in chat"),
//! })
//! .await?;
//! # Ok(())
//! # }
//! ```
//!
//! # Identity
//!
//! There are no numeric user IDs. A username is a string the customer already has, and
//! a device is derived from the device key's thumbprint, so a device ban and the
//! thumbprint passed to [`Client::mint_token`] are the same fact.
//!
//! # Retries
//!
//! Every mutating call carries an idempotency key, generated here when the caller does
//! not supply one, and a retry reuses it -- so a queue that redelivers "ban for 24
//! hours" does not extend the ban. Supply your own key in [`Actor`] when the retry might
//! come from a different process than the first attempt, since a key generated here
//! cannot outlive it.

mod client;
mod convert;
mod error;
mod events;
mod grants;
mod moderation;
mod rooms;
mod tokens;
mod types;

/// The generated protobuf bindings.
///
/// Public because a caller with an unusual need -- an interceptor, a custom channel, a
/// service this SDK has not wrapped yet -- should not be blocked by this crate's
/// coverage. Everything here is generated from `proto/moqom/admin/v1`, and the wrapped
/// surface is what carries the compatibility promise.
pub mod proto {
    #![allow(missing_docs, clippy::all, clippy::pedantic)]
    include!("generated/moqom.admin.v1.rs");
}

pub use client::{Client, ClientBuilder, Room};
pub use error::{Error, Result};
pub use events::{
    Body, Event, EventKind, EventQuery, EventStream, GrantsChanged, LifecycleChanged, MuteChanged,
    ParticipantJoined, ParticipantLeft, PresenceChanged, PublishChanged, SpotlightChanged,
    WatchdogChanged,
};
pub use grants::{Capability, Grants, Preset, Publish, Role};
pub use moderation::{
    BanRequest, GrantsRequest, KickRequest, LogQuery, MuteRequest, banned_account, banned_address,
    banned_device,
};
pub use rooms::{NewRoom, NewWatchdog, ParticipantQuery, PolicyUpdate, RoomQuery};
pub use tokens::{TokenRequest, VerificationKeys};
pub use types::{
    Actor, AgeGate, BanScope, BanSubject, Binding, Code, Entry, Identity, Lifecycle, Media,
    MuteScope, Page, Participant, ParticipantRef, Policy, Presence, RecordingPolicy, RoomState,
    Token, Verb, VerificationKey, Watchdog, WatchdogHealth, WatchdogKind, WatchdogOperator, device,
    target,
};
