//! The Rust SDK against the real control plane.
//!
//! The stub tests in `admin.rs` check that this crate agrees with itself. This checks the
//! thing they cannot: that it agrees with the server. Those are different failures, and
//! the second one is the expensive kind -- a capability bit that means `KICK` here and
//! `BAN_FROM_TENANT` there would pass every test in this crate.
//!
//! It needs a control plane binary and therefore a Go toolchain, so every test here is
//! `#[ignore]` and `cargo test` reports them as skipped rather than failing on a machine
//! that has no server to talk to. Run them with:
//!
//! ```text
//! tools/interop.sh
//! ```
//!
//! which builds `moqom-control`, points `MOQOM_CONTROL_BIN` at it, and passes
//! `--ignored`. Asked to run without that variable, these panic rather than skipping: a
//! cross-language check that quietly excuses itself is worse than no check, because it
//! puts a green tick over an untested seam. A run that does not name this file is not
//! evidence of interop.

use std::io::{BufRead, BufReader};
use std::process::{Child, Command, Stdio};
use std::time::Duration;

use moqom_sdk::{
    Actor, BanRequest, Body, Capability, Client, Code, Error, EventQuery, Grants, GrantsRequest,
    KickRequest, NewRoom, Policy, Preset, Publish, TokenRequest, banned_account, target,
};

const CREDENTIAL: &str = "interop-owner-key";
const TENANT: &str = "acme";

/// A control plane on a loopback port, killed when this is dropped.
struct ControlPlane {
    process: Child,
    address: String,
    _accounts: TempFile,
}

impl ControlPlane {
    /// Start one, or return why it could not be started.
    fn start() -> std::result::Result<Self, String> {
        let binary = std::env::var("MOQOM_CONTROL_BIN").map_err(|_| {
            "MOQOM_CONTROL_BIN is not set; run tools/interop.sh to build the control plane \
             and run this test against it"
                .to_owned()
        })?;

        let accounts = TempFile::write(
            "moqom-interop-accounts.json",
            &format!(
                r#"{{"accounts":[{{
                    "name":"interop-owner",
                    "credential":"{CREDENTIAL}",
                    "tenant":"{TENANT}",
                    "publish":["audio","video","screen_share","data"],
                    "preset":"owner",
                    "is_owner":true
                }}]}}"#
            ),
        )?;

        let mut process = Command::new(&binary)
            // Port zero, and the address is read back from the log. Reserving a port
            // first would race with anything else on the machine doing the same.
            .env("MOQOM_CONTROL_ADDR", "127.0.0.1:0")
            .env("MOQOM_CONTROL_ACCOUNTS", accounts.path())
            .env("MOQOM_CONTROL_STORE", "memory")
            .env("MOQOM_CONTROL_ISSUER", "moqom-interop")
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .map_err(|e| format!("starting {binary}: {e}"))?;

        let stderr = process
            .stderr
            .take()
            .ok_or_else(|| "the control plane has no stderr".to_owned())?;

        // Drained on a thread, and drained to the end rather than until the address is
        // found. A Go process that writes to a closed stderr dies of SIGPIPE, so stopping
        // early would kill the server at its next log line -- which is a confusing way to
        // fail, since the test would already be several calls in by then.
        let (found, addresses) = std::sync::mpsc::channel();
        std::thread::spawn(move || {
            for line in BufReader::new(stderr).lines().map_while(Result::ok) {
                if let Some(address) = listening_address(&line) {
                    let _ = found.send(address);
                }
            }
        });

        let Ok(address) = addresses.recv_timeout(Duration::from_secs(20)) else {
            let _ = process.kill();
            return Err(
                "the control plane did not report a listening address within 20s".to_owned(),
            );
        };

        Ok(Self {
            process,
            address,
            _accounts: accounts,
        })
    }

    async fn client(&self) -> Client {
        Client::builder(format!("http://{}", self.address))
            .tenant(TENANT)
            .credential(CREDENTIAL)
            // The control plane expects TLS termination in front of it, which there is
            // not one of here. This is the case the escape hatch exists for.
            .allow_plaintext()
            .retry(3, Duration::from_millis(10))
            .connect()
            .await
            .expect("connecting to the control plane")
    }
}

impl Drop for ControlPlane {
    fn drop(&mut self) {
        let _ = self.process.kill();
        let _ = self.process.wait();
    }
}

/// Pulls the bound address out of the control plane's structured log.
///
/// Matched on the message and the field rather than parsed as JSON, to keep a JSON
/// dependency out of a test that needs one string.
fn listening_address(line: &str) -> Option<String> {
    if !line.contains("control plane listening") {
        return None;
    }
    let start = line.find(r#""address":""#)? + r#""address":""#.len();
    let rest = &line[start..];
    let end = rest.find('"')?;
    Some(rest[..end].to_owned())
}

/// A file that removes itself.
struct TempFile(std::path::PathBuf);

impl TempFile {
    fn write(name: &str, contents: &str) -> std::result::Result<Self, String> {
        let path = std::env::temp_dir().join(format!("{}-{name}", std::process::id()));
        std::fs::write(&path, contents).map_err(|e| format!("writing {}: {e}", path.display()))?;
        Ok(Self(path))
    }

    fn path(&self) -> &std::path::Path {
        &self.0
    }
}

impl Drop for TempFile {
    fn drop(&mut self) {
        let _ = std::fs::remove_file(&self.0);
    }
}

/// Everything a backend does, against the server that will do it in production.
#[tokio::test]
#[ignore = "needs a control plane binary; run tools/interop.sh"]
async fn the_rust_sdk_runs_a_room() {
    let control = match ControlPlane::start() {
        Ok(control) => control,
        Err(why) => panic!("{why}"),
    };
    let client = control.client().await;

    let room = client
        .create_room(NewRoom {
            name: "rust-room".into(),
            owner: "alice".into(),
            policy: Policy {
                residency_zone: "eu".into(),
                stage_slots: 4,
                ..Policy::default()
            },
            actor: Actor::on_behalf_of("alice").because("interop"),
            ..NewRoom::default()
        })
        .await
        .expect("creating a room");

    assert_eq!(room.namespace, format!("{TENANT}/r/rust-room"));
    assert_eq!(room.policy.stage_slots, 4);
    assert_eq!(room.policy.residency_zone, "eu");
    assert_eq!(room.lifecycle, moqom_sdk::Lifecycle::Live);

    let handle = client.room("rust-room");

    // The call a backend makes most.
    let token = client
        .mint_token(TokenRequest {
            username: "bob".into(),
            device_thumbprint: "thumbprint-of-bobs-phone".into(),
            room: "rust-room".into(),
            publish: Publish::CAMERA,
            preset: Some(Preset::Moderator),
            lifetime: Duration::from_mins(5),
            ..TokenRequest::default()
        })
        .await
        .expect("minting");

    assert!(!token.token.is_empty(), "an empty token came back");
    assert_eq!(token.identity.username, "bob");
    assert!(
        !token.identity.device.is_empty(),
        "the device was not derived from the thumbprint"
    );

    let keys = client
        .verification_keys()
        .await
        .expect("reading verification keys");
    assert_eq!(
        keys.keys.first().map(|key| key.key_id.as_str()),
        Some(token.key_id.as_str()),
        "the key that signed the token is not published"
    );
    assert_eq!(keys.keys[0].algorithm, "EdDSA");

    // Moderation, and the record of it.
    let entry = handle
        .ban(
            BanRequest::lasting(banned_account("carol"), Duration::from_hours(24)).coded(
                Code::Harassment,
                Actor::on_behalf_of("alice").because("slurs in chat"),
            ),
        )
        .await
        .expect("banning");

    assert_eq!(entry.verb, moqom_sdk::Verb::Ban);
    assert_eq!(entry.ban_scope, moqom_sdk::BanScope::Room);
    assert_eq!(entry.on_behalf_of, "alice");
    assert_eq!(entry.reason, "slurs in chat");
    assert_eq!(
        entry.service_account, "interop-owner",
        "the service account was not filled from the credentials"
    );
    assert!(
        entry.expires_at.is_some(),
        "a 24-hour ban came back with no expiry"
    );
    assert!(
        entry.enforced,
        "a ban with no affected sessions reported incomplete enforcement"
    );

    // The audit trail.
    let log = handle
        .log(moqom_sdk::LogQuery::default())
        .await
        .expect("reading the log");
    assert_eq!(log.items.len(), 1);
    assert_eq!(log.items[0].id, entry.id);

    // Closing, and what happens after.
    let closed = handle
        .close(Code::OperatorAction, Actor::on_behalf_of("alice"))
        .await
        .expect("closing");
    assert_eq!(closed.lifecycle, moqom_sdk::Lifecycle::Ended);

    let refused = client
        .mint_token(TokenRequest {
            username: "dave".into(),
            device_thumbprint: "thumbprint-of-daves-phone".into(),
            room: "rust-room".into(),
            ..TokenRequest::default()
        })
        .await;
    assert!(
        matches!(refused, Err(Error::Closed { .. })),
        "minting into a closed room gave {refused:?}"
    );
}

/// The check this whole file exists for.
///
/// A capability is a bit here and an enum value on the wire, and the server maps that
/// enum back to its own bit. If the two tables disagree anywhere, a moderator ends up
/// holding a power somebody meant to withhold -- so every capability is sent alone and
/// read back, which is the only way to catch a table where two rows point at the same
/// wire value.
#[tokio::test]
#[ignore = "needs a control plane binary; run tools/interop.sh"]
async fn capabilities_mean_the_same_thing_on_both_sides() {
    let control = match ControlPlane::start() {
        Ok(control) => control,
        Err(why) => panic!("{why}"),
    };
    let client = control.client().await;

    client
        .create_room(NewRoom {
            name: "bits".into(),
            owner: "alice".into(),
            ..NewRoom::default()
        })
        .await
        .expect("creating a room");

    let handle = client.room("bits");

    for capability in [
        Capability::DELETE_MESSAGE,
        Capability::TIMEOUT_USER,
        Capability::BAN_FROM_CHAT,
        Capability::SET_CHAT_MODE,
        Capability::MANAGE_FILTERS,
        Capability::PROMOTE_TO_GUEST,
        Capability::PROMOTE_TO_COHOST,
        Capability::DEMOTE,
        Capability::FORCE_MUTE,
        Capability::SPOTLIGHT,
        Capability::MANAGE_QUEUE,
        Capability::LOCK_STAGE,
        Capability::KICK,
        Capability::BAN_FROM_ROOM,
        Capability::BAN_FROM_TENANT,
        Capability::CLOSE_ROOM,
        Capability::CHANGE_ROOM_POLICY,
        Capability::MANAGE_WATCHDOGS,
        Capability::APPOINT_MODERATORS,
        Capability::READ_MODERATION_LOG,
        Capability::HANDLE_APPEALS,
    ] {
        let entry = handle
            .set_grants(GrantsRequest {
                target: target("carol"),
                grants: Grants {
                    moderation: capability,
                    ..Grants::default()
                },
                ..GrantsRequest::default()
            })
            .await
            .unwrap_or_else(|e| panic!("granting {capability:?}: {e}"));

        assert_eq!(
            entry.grants.moderation, capability,
            "{capability:?} came back from the server as {:?}",
            entry.grants.moderation
        );
    }

    // And the publish scopes, which have their own table.
    for scope in [
        Publish::AUDIO,
        Publish::VIDEO,
        Publish::SCREEN_SHARE,
        Publish::DATA,
    ] {
        let entry = handle
            .set_grants(GrantsRequest {
                target: target("carol"),
                grants: Grants {
                    publish: scope,
                    ..Grants::default()
                },
                ..GrantsRequest::default()
            })
            .await
            .unwrap_or_else(|e| panic!("granting {scope:?}: {e}"));

        assert_eq!(
            entry.grants.publish, scope,
            "{scope:?} came back from the server as {:?}",
            entry.grants.publish
        );
    }
}

/// The event stream, which is a backend's substitute for being in the room.
#[tokio::test]
#[ignore = "needs a control plane binary; run tools/interop.sh"]
async fn the_rust_sdk_follows_a_room() {
    let control = match ControlPlane::start() {
        Ok(control) => control,
        Err(why) => panic!("{why}"),
    };
    let client = control.client().await;

    client
        .create_room(NewRoom {
            name: "watched".into(),
            owner: "alice".into(),
            ..NewRoom::default()
        })
        .await
        .expect("creating a room");

    let handle = client.room("watched");
    let mut stream = handle
        .events(EventQuery::default())
        .await
        .expect("opening the stream");

    let kicked = handle
        .kick(KickRequest {
            target: target("carol"),
            code: Code::OperatorAction,
            actor: Actor::on_behalf_of("alice"),
        })
        .await
        .expect("kicking");

    let event = stream
        .next()
        .await
        .expect("reading the stream")
        .expect("an event arrives");

    assert_eq!(event.kind, moqom_sdk::EventKind::ModerationApplied);
    assert!(
        event.sequence > 0,
        "the event has no sequence number, so a consumer cannot resume"
    );
    match event.body {
        Body::Moderation(entry) => assert_eq!(entry.id, kicked.id),
        other => panic!("expected the moderation entry, got {other:?}"),
    }

    let cursor = event.sequence;
    drop(stream);

    // Something happens while nobody is listening.
    let second = handle
        .kick(KickRequest {
            target: target("dave"),
            actor: Actor::on_behalf_of("alice"),
            ..KickRequest::default()
        })
        .await
        .expect("kicking again");

    // A consumer that persisted its cursor sees it anyway, which is the difference
    // between a dashboard and something that must not miss an event.
    let mut resumed = handle
        .events(EventQuery {
            after: cursor,
            ..EventQuery::default()
        })
        .await
        .expect("resuming");

    let missed = resumed
        .next()
        .await
        .expect("reading after resuming")
        .expect("the missed event arrives");

    assert!(
        missed.sequence > cursor,
        "resuming replayed something already seen"
    );
    match missed.body {
        Body::Moderation(entry) => assert_eq!(entry.id, second.id),
        other => panic!("expected the missed moderation entry, got {other:?}"),
    }
}
