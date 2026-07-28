//! The SDK against a stub control plane.
//!
//! The stub does as it is told, which is the point: these tests are about the SDK, and
//! they can provoke failures a working server never produces. What they cannot check is
//! whether the two agree about anything, so the counterpart lives in the monorepo, where
//! the real control plane answers.

use std::sync::{Arc, Mutex};
use std::time::Duration;

use moqom_sdk::proto;
use moqom_sdk::{
    Actor, BanRequest, BanSubject, Body, Capability, Client, Code, Error, EventQuery, Grants,
    GrantsRequest, Identity, KickRequest, Media, MuteRequest, MuteScope, NewRoom, Preset, Publish,
    TokenRequest, banned_account, banned_address, target,
};
use tokio::net::TcpListener;
use tonic::transport::{Channel, Server};
use tonic::{Request, Response, Status};

/// What the stub recorded and what it will do next.
#[derive(Default)]
struct Script {
    /// Every idempotency key the stub has seen, in order.
    keys: Vec<String>,

    /// Every authorization header value it has seen.
    credentials: Vec<String>,

    /// How many calls to fail before succeeding, and with what.
    fail: Option<(u32, tonic::Code)>,

    /// Calls served so far.
    calls: u32,

    /// The last `SetGrants` request, for the round-trip check.
    grants: Option<proto::SetGrantsRequest>,

    /// Events to deliver on the next stream.
    events: Vec<proto::RoomEvent>,
}

#[derive(Clone, Default)]
struct Stub {
    script: Arc<Mutex<Script>>,
}

impl Stub {
    fn record<T>(&self, request: &Request<T>, key: Option<&proto::ActorContext>) {
        let mut script = self.script.lock().expect("the stub's lock is not poisoned");
        script.calls += 1;
        if let Some(actor) = key {
            script.keys.push(actor.idempotency_key.clone());
        }
        if let Some(value) = request.metadata().get("authorization") {
            script
                .credentials
                .push(value.to_str().unwrap_or("<not ascii>").to_owned());
        }
    }

    /// Whether this call should fail, honouring the scripted count.
    fn should_fail(&self) -> Option<Status> {
        let mut script = self.script.lock().expect("the stub's lock is not poisoned");
        let (remaining, code) = script.fail?;
        if remaining == 0 {
            return None;
        }
        script.fail = Some((remaining - 1, code));
        Some(Status::new(code, "as scripted"))
    }
}

#[tonic::async_trait]
impl proto::moderation_server::Moderation for Stub {
    async fn kick(
        &self,
        request: Request<proto::KickRequest>,
    ) -> Result<Response<proto::ModerationEntry>, Status> {
        self.record(&request, request.get_ref().actor.as_ref());
        if let Some(status) = self.should_fail() {
            return Err(status);
        }
        Ok(Response::new(proto::ModerationEntry {
            id: "entry".into(),
            verb: proto::ModerationVerb::Kick as i32,
            enforced: true,
            ..proto::ModerationEntry::default()
        }))
    }

    async fn ban(
        &self,
        request: Request<proto::BanRequest>,
    ) -> Result<Response<proto::ModerationEntry>, Status> {
        self.record(&request, request.get_ref().actor.as_ref());
        if let Some(status) = self.should_fail() {
            return Err(status);
        }
        let inner = request.into_inner();
        Ok(Response::new(proto::ModerationEntry {
            id: "ban".into(),
            verb: proto::ModerationVerb::Ban as i32,
            ban_scope: inner.scope,
            ban_subject: inner.subject,
            ..proto::ModerationEntry::default()
        }))
    }

    async fn unban(
        &self,
        _: Request<proto::UnbanRequest>,
    ) -> Result<Response<proto::ModerationEntry>, Status> {
        Err(Status::unimplemented("not used by these tests"))
    }

    async fn force_mute(
        &self,
        request: Request<proto::ForceMuteRequest>,
    ) -> Result<Response<proto::ModerationEntry>, Status> {
        let inner = request.into_inner();
        Ok(Response::new(proto::ModerationEntry {
            id: "mute".into(),
            verb: proto::ModerationVerb::ForceMute as i32,
            mute_scope: inner.scope,
            media: inner.media,
            ..proto::ModerationEntry::default()
        }))
    }

    async fn unmute(
        &self,
        _: Request<proto::UnmuteRequest>,
    ) -> Result<Response<proto::ModerationEntry>, Status> {
        Err(Status::unimplemented("not used by these tests"))
    }

    async fn set_grants(
        &self,
        request: Request<proto::SetGrantsRequest>,
    ) -> Result<Response<proto::ModerationEntry>, Status> {
        let inner = request.into_inner();
        self.script
            .lock()
            .expect("the stub's lock is not poisoned")
            .grants = Some(inner.clone());
        Ok(Response::new(proto::ModerationEntry {
            verb: proto::ModerationVerb::SetGrants as i32,
            grants: inner.grants,
            ..proto::ModerationEntry::default()
        }))
    }

    async fn spotlight(
        &self,
        _: Request<proto::SpotlightRequest>,
    ) -> Result<Response<proto::ModerationEntry>, Status> {
        Err(Status::unimplemented("not used by these tests"))
    }

    async fn list_moderation_log(
        &self,
        _: Request<proto::ListModerationLogRequest>,
    ) -> Result<Response<proto::ListModerationLogResponse>, Status> {
        Err(Status::unimplemented("not used by these tests"))
    }
}

#[tonic::async_trait]
impl proto::rooms_server::Rooms for Stub {
    async fn create_room(
        &self,
        request: Request<proto::CreateRoomRequest>,
    ) -> Result<Response<proto::Room>, Status> {
        let inner = request.into_inner();
        Ok(Response::new(proto::Room {
            tenant: inner.tenant.clone(),
            namespace: format!("{}/r/{}", inner.tenant, inner.room),
            room: inner.room,
            policy: inner.policy,
            lifecycle: proto::RoomLifecycle::Live as i32,
            ..proto::Room::default()
        }))
    }

    async fn get_room(
        &self,
        request: Request<proto::GetRoomRequest>,
    ) -> Result<Response<proto::Room>, Status> {
        let inner = request.into_inner();
        if inner.room == "missing" {
            return Err(Status::not_found("no such room"));
        }
        Ok(Response::new(proto::Room {
            tenant: inner.tenant.clone(),
            namespace: format!("{}/r/{}", inner.tenant, inner.room),
            room: inner.room,
            lifecycle: proto::RoomLifecycle::Live as i32,
            policy: Some(proto::RoomPolicy {
                recording: proto::RecordingPolicy::Allowed as i32,
                stage_slots: 8,
                age_gate: Some(proto::AgeGate {
                    minimum_age: 18,
                    require_verification: true,
                }),
                ..proto::RoomPolicy::default()
            }),
            ..proto::Room::default()
        }))
    }

    async fn list_rooms(
        &self,
        _: Request<proto::ListRoomsRequest>,
    ) -> Result<Response<proto::ListRoomsResponse>, Status> {
        Ok(Response::new(proto::ListRoomsResponse::default()))
    }

    async fn update_room_policy(
        &self,
        _: Request<proto::UpdateRoomPolicyRequest>,
    ) -> Result<Response<proto::Room>, Status> {
        Err(Status::unimplemented("not used by these tests"))
    }

    async fn close_room(
        &self,
        _: Request<proto::CloseRoomRequest>,
    ) -> Result<Response<proto::Room>, Status> {
        Err(Status::unimplemented("not used by these tests"))
    }

    async fn list_participants(
        &self,
        _: Request<proto::ListParticipantsRequest>,
    ) -> Result<Response<proto::ListParticipantsResponse>, Status> {
        Ok(Response::new(proto::ListParticipantsResponse::default()))
    }

    async fn get_participant(
        &self,
        _: Request<proto::GetParticipantRequest>,
    ) -> Result<Response<proto::Participant>, Status> {
        Err(Status::unimplemented("not used by these tests"))
    }

    async fn attach_watchdog(
        &self,
        _: Request<proto::AttachWatchdogRequest>,
    ) -> Result<Response<proto::Watchdog>, Status> {
        Err(Status::unimplemented("not used by these tests"))
    }

    async fn detach_watchdog(
        &self,
        _: Request<proto::DetachWatchdogRequest>,
    ) -> Result<Response<proto::Watchdog>, Status> {
        Err(Status::unimplemented("not used by these tests"))
    }

    async fn list_watchdogs(
        &self,
        _: Request<proto::ListWatchdogsRequest>,
    ) -> Result<Response<proto::ListWatchdogsResponse>, Status> {
        Ok(Response::new(proto::ListWatchdogsResponse::default()))
    }
}

#[tonic::async_trait]
impl proto::tokens_server::Tokens for Stub {
    async fn mint_client_token(
        &self,
        request: Request<proto::MintClientTokenRequest>,
    ) -> Result<Response<proto::MintClientTokenResponse>, Status> {
        let inner = request.into_inner();
        Ok(Response::new(proto::MintClientTokenResponse {
            token: "a.b.c".into(),
            key_id: "key".into(),
            identity: Some(proto::Identity {
                username: inner.username,
                device: "derived".into(),
            }),
            ..proto::MintClientTokenResponse::default()
        }))
    }

    async fn list_verification_keys(
        &self,
        _: Request<proto::ListVerificationKeysRequest>,
    ) -> Result<Response<proto::ListVerificationKeysResponse>, Status> {
        Ok(Response::new(proto::ListVerificationKeysResponse {
            keys: vec![proto::VerificationKey {
                key_id: "key".into(),
                algorithm: "EdDSA".into(),
                jwk: "{}".into(),
                ..proto::VerificationKey::default()
            }],
            binding: proto::TokenBinding::Bearer as i32,
        }))
    }
}

#[tonic::async_trait]
impl proto::events_server::Events for Stub {
    type StreamRoomEventsStream = std::pin::Pin<
        Box<dyn tokio_stream::Stream<Item = Result<proto::RoomEvent, Status>> + Send>,
    >;

    async fn stream_room_events(
        &self,
        _: Request<proto::StreamRoomEventsRequest>,
    ) -> Result<Response<Self::StreamRoomEventsStream>, Status> {
        let events = self
            .script
            .lock()
            .expect("the stub's lock is not poisoned")
            .events
            .clone();
        let stream = tokio_stream::iter(events.into_iter().map(Ok));
        Ok(Response::new(Box::pin(stream)))
    }
}

/// Serves the stub and returns a client pointed at it.
async fn serve(stub: Stub) -> Client {
    let listener = TcpListener::bind("127.0.0.1:0")
        .await
        .expect("binding a loopback port");
    let address = listener.local_addr().expect("a bound address");

    tokio::spawn(async move {
        let _ = Server::builder()
            .add_service(proto::rooms_server::RoomsServer::new(stub.clone()))
            .add_service(proto::moderation_server::ModerationServer::new(
                stub.clone(),
            ))
            .add_service(proto::tokens_server::TokensServer::new(stub.clone()))
            .add_service(proto::events_server::EventsServer::new(stub))
            .serve_with_incoming(tokio_stream::wrappers::TcpListenerStream::new(listener))
            .await;
    });

    let channel = Channel::from_shared(format!("http://{address}"))
        .expect("a usable endpoint")
        .connect()
        .await
        .expect("connecting to the stub");

    Client::builder("unused")
        .tenant("acme")
        .credential("secret")
        .retry(3, Duration::from_millis(1))
        .with_channel(channel)
        .expect("building the client")
}

#[tokio::test]
async fn a_client_needs_a_tenant_and_a_credential() {
    let missing_tenant = Client::builder("https://api.moqom.cloud")
        .credential("k")
        .connect()
        .await;
    assert!(
        matches!(missing_tenant, Err(Error::Request(_))),
        "a client without a tenant was accepted; every call names its tenant on the wire"
    );

    let missing_credential = Client::builder("https://api.moqom.cloud")
        .tenant("acme")
        .connect()
        .await;
    assert!(
        matches!(missing_credential, Err(Error::Request(_))),
        "a client without a credential was accepted"
    );
}

/// The credential is a bearer token granting everything the service account can do, so an
/// `http://` endpoint has to be an explicit decision rather than a typo that works.
#[tokio::test]
async fn plaintext_is_refused_unless_it_is_asked_for() {
    let refused = Client::builder("http://api.moqom.cloud")
        .tenant("acme")
        .credential("secret")
        .connect()
        .await;

    match refused {
        Err(Error::Request(message)) => assert!(
            message.contains("in the clear"),
            "expected a refusal about sending the credential in the clear, got: {message}"
        ),
        other => panic!("plaintext was accepted without being asked for: {other:?}"),
    }
}

#[tokio::test]
async fn the_credential_is_sent_as_a_bearer_token() {
    let stub = Stub::default();
    let client = serve(stub.clone()).await;

    client
        .room("live")
        .kick(KickRequest {
            target: target("carol"),
            ..KickRequest::default()
        })
        .await
        .expect("kicking");

    let script = stub.script.lock().expect("the stub's lock is not poisoned");
    assert_eq!(
        script.credentials,
        vec!["Bearer secret".to_owned()],
        "the credential did not arrive as a bearer token"
    );
}

/// The property the whole retry design exists for. A queue that redelivers "ban for 24
/// hours" must not extend the ban, which only holds if every attempt carries the same key
/// -- so generating one per attempt would defeat it silently, and silently is why this is
/// a test rather than a comment.
#[tokio::test]
async fn a_retry_reuses_its_idempotency_key() {
    let stub = Stub::default();
    stub.script
        .lock()
        .expect("the stub's lock is not poisoned")
        .fail = Some((2, tonic::Code::Unavailable));

    let client = serve(stub.clone()).await;
    let entry = client
        .room("live")
        .kick(KickRequest {
            target: target("carol"),
            code: Code::Harassment,
            actor: Actor::on_behalf_of("jane"),
        })
        .await
        .expect("the third attempt succeeds");

    assert_eq!(entry.id, "entry");

    let script = stub.script.lock().expect("the stub's lock is not poisoned");
    assert_eq!(script.keys.len(), 3, "expected three attempts");
    assert!(
        !script.keys[0].is_empty(),
        "no idempotency key was generated"
    );
    assert!(
        script.keys.iter().all(|key| *key == script.keys[0]),
        "attempts used different keys: {:?}",
        script.keys
    );
}

/// A caller's own key has to win, because a key generated inside this process cannot
/// survive the process. A worker that wants "this message, however many times it is
/// delivered" derives one from the message.
#[tokio::test]
async fn a_caller_supplied_key_is_used_verbatim() {
    let stub = Stub::default();
    let client = serve(stub.clone()).await;

    client
        .room("live")
        .ban(
            BanRequest::lasting(banned_account("carol"), Duration::from_hours(1)).coded(
                Code::Harassment,
                Actor::on_behalf_of("jane").keyed("queue-91"),
            ),
        )
        .await
        .expect("banning");

    let script = stub.script.lock().expect("the stub's lock is not poisoned");
    assert_eq!(script.keys, vec!["queue-91".to_owned()]);
}

/// Retrying a permission failure is how a moderation tool ends up hammering a door it will
/// never be let through, so only codes that cannot have applied a change are retried.
#[tokio::test]
async fn only_transient_failures_are_retried() {
    for (code, expected) in [
        (tonic::Code::PermissionDenied, 1),
        (tonic::Code::InvalidArgument, 1),
        (tonic::Code::NotFound, 1),
        (tonic::Code::Unavailable, 3),
    ] {
        let stub = Stub::default();
        stub.script
            .lock()
            .expect("the stub's lock is not poisoned")
            .fail = Some((u32::MAX, code));

        let client = serve(stub.clone()).await;
        let failed = client
            .room("live")
            .kick(KickRequest {
                target: target("carol"),
                ..KickRequest::default()
            })
            .await;
        assert!(failed.is_err(), "{code} should have failed");

        let calls = stub
            .script
            .lock()
            .expect("the stub's lock is not poisoned")
            .calls;
        assert_eq!(calls, expected, "{code} was tried {calls} times");
    }
}

/// A caller should be able to match on what went wrong, and the operation should be in the
/// message: "permission denied" with no verb is the kind of log line that costs an hour.
#[tokio::test]
async fn failures_map_onto_variants_that_name_the_operation() {
    for (code, matches) in [
        (
            tonic::Code::PermissionDenied,
            (|e: &Error| matches!(e, Error::Denied { .. })) as fn(&Error) -> bool,
        ),
        (tonic::Code::NotFound, |e| {
            matches!(e, Error::NotFound { .. })
        }),
        (tonic::Code::InvalidArgument, |e| {
            matches!(e, Error::Invalid { .. })
        }),
        (tonic::Code::Unauthenticated, |e| {
            matches!(e, Error::Unauthenticated { .. })
        }),
        (tonic::Code::AlreadyExists, |e| {
            matches!(e, Error::Exists { .. })
        }),
        (tonic::Code::FailedPrecondition, |e| {
            matches!(e, Error::Closed { .. })
        }),
        (tonic::Code::Unavailable, |e| {
            matches!(e, Error::Unavailable { .. })
        }),
        (tonic::Code::Internal, |e| {
            matches!(e, Error::Internal { .. })
        }),
    ] {
        let stub = Stub::default();
        stub.script
            .lock()
            .expect("the stub's lock is not poisoned")
            .fail = Some((u32::MAX, code));

        let client = serve(stub).await;
        let error = client
            .room("live")
            .kick(KickRequest {
                target: target("carol"),
                ..KickRequest::default()
            })
            .await
            .expect_err("the call should fail");

        assert!(matches(&error), "{code} produced {error:?}");
        assert_eq!(error.operation(), "room.kick", "the operation was lost");
        assert!(
            error.retryable() == (code == tonic::Code::Unavailable),
            "{code} reported the wrong retryability"
        );
    }
}

/// Malformed requests are refused here rather than on the wire. Not for speed: a ban with
/// no subject is ambiguous, and the round trip would only report the same thing later.
#[tokio::test]
async fn obviously_malformed_requests_are_refused_locally() {
    let client = serve(Stub::default()).await;
    let room = client.room("live");

    assert!(
        room.kick(KickRequest::default()).await.is_err(),
        "a kick with no target was accepted"
    );
    assert!(
        room.force_mute(MuteRequest {
            target: target("carol"),
            ..MuteRequest::default()
        })
        .await
        .is_err(),
        "a mute with no scope was accepted; the scope is which mute, not a detail"
    );
    assert!(
        client.create_room(NewRoom::default()).await.is_err(),
        "a room with no name was accepted"
    );
    assert!(
        room.participant(&Identity {
            username: "carol".into(),
            device: String::new()
        })
        .await
        .is_err(),
        "a participant read with no device was accepted; that question's answer is a list"
    );
    assert!(
        client
            .mint_token(TokenRequest {
                username: "bob".into(),
                ..TokenRequest::default()
            })
            .await
            .is_err(),
        "minting without a device thumbprint was accepted"
    );
}

#[tokio::test]
async fn reads_come_back_as_domain_types() {
    let client = serve(Stub::default()).await;

    let room = client.room("live").get().await.expect("reading the room");
    assert_eq!(room.namespace, "acme/r/live");
    assert_eq!(room.lifecycle, moqom_sdk::Lifecycle::Live);
    assert_eq!(room.policy.stage_slots, 8);
    assert_eq!(room.policy.age_gate.minimum_age, 18);
    assert!(room.policy.age_gate.require_verification);

    // An unset timestamp has to arrive as None rather than the epoch: an ended-at of 1970
    // on a live room would make every one of them look finished.
    assert!(
        room.ended_at.is_none(),
        "an absent timestamp became {:?}",
        room.ended_at
    );

    let missing = client.room("missing").get().await;
    assert!(matches!(missing, Err(Error::NotFound { .. })));
}

/// Capabilities are a bitmask here and a repeated enum on the wire, so the translation is
/// a place a bit could go missing. Every one is checked, in both directions and one at a
/// time -- a table with two rows pointing at the same wire value would survive a test that
/// only sent the full set.
#[tokio::test]
async fn every_capability_and_scope_survives_the_wire() {
    let stub = Stub::default();
    let client = serve(stub.clone()).await;
    let room = client.room("live");

    let everything = Grants {
        publish: Publish::EVERYTHING,
        moderation: Capability::EVERYTHING,
        is_owner: false,
    };
    let entry = room
        .set_grants(GrantsRequest {
            target: target("carol"),
            grants: everything,
            ..GrantsRequest::default()
        })
        .await
        .expect("setting grants");

    {
        let script = stub.script.lock().expect("the stub's lock is not poisoned");
        let sent = script.grants.as_ref().expect("the stub saw the request");
        let wire = sent.grants.as_ref().expect("grants were sent");
        assert_eq!(wire.moderation.len(), 21, "a capability went missing");
        assert_eq!(wire.publish.len(), 4, "a publish scope went missing");
    }
    assert_eq!(
        entry.grants, everything,
        "grants did not survive the round trip"
    );

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
        let entry = room
            .set_grants(GrantsRequest {
                target: target("carol"),
                grants: Grants {
                    moderation: capability,
                    ..Grants::default()
                },
                ..GrantsRequest::default()
            })
            .await
            .expect("setting one capability");
        assert_eq!(
            entry.grants.moderation, capability,
            "{capability:?} came back as {:?}",
            entry.grants.moderation
        );
    }
}

#[tokio::test]
async fn a_ban_carries_its_subject_and_scope() {
    let client = serve(Stub::default()).await;

    let entry = client
        .room("live")
        .ban(BanRequest::permanent(banned_account("carol")))
        .await
        .expect("banning");
    assert_eq!(entry.ban_scope, moqom_sdk::BanScope::Room);
    assert_eq!(
        entry.ban_subject,
        Some(BanSubject::Account("carol".into())),
        "the subject did not survive"
    );

    let wide = client
        .ban_from_tenant(BanRequest::permanent(banned_address("10.0.0.0/8")))
        .await
        .expect("banning tenant-wide");
    assert_eq!(wide.ban_scope, moqom_sdk::BanScope::Tenant);
    assert_eq!(
        wide.ban_subject,
        Some(BanSubject::Address("10.0.0.0/8".into()))
    );
}

#[tokio::test]
async fn a_mute_carries_its_scope_and_media() {
    let client = serve(Stub::default()).await;

    let entry = client
        .room("live")
        .force_mute(MuteRequest {
            target: target("carol"),
            media: vec![Media::Audio],
            scope: MuteScope::Audience,
            duration: Duration::from_mins(10),
            actor: Actor::on_behalf_of("jane"),
        })
        .await
        .expect("muting");

    assert_eq!(entry.mute_scope, MuteScope::Audience);
    assert_eq!(entry.media, vec![Media::Audio]);
}

#[tokio::test]
async fn events_arrive_as_matchable_bodies() {
    let stub = Stub::default();
    stub.script
        .lock()
        .expect("the stub's lock is not poisoned")
        .events = vec![
        proto::RoomEvent {
            tenant: "acme".into(),
            room: "live".into(),
            sequence: 7,
            kind: proto::RoomEventKind::ParticipantJoined as i32,
            body: Some(proto::room_event::Body::ParticipantJoined(
                proto::ParticipantJoined {
                    participant: Some(proto::Participant {
                        identity: Some(proto::Identity {
                            username: "carol".into(),
                            device: "phone".into(),
                        }),
                        presence: proto::Presence::Connected as i32,
                        ..proto::Participant::default()
                    }),
                    first_device: true,
                },
            )),
            ..proto::RoomEvent::default()
        },
        proto::RoomEvent {
            tenant: "acme".into(),
            room: "live".into(),
            sequence: 8,
            kind: proto::RoomEventKind::ModerationApplied as i32,
            body: Some(proto::room_event::Body::ModerationApplied(
                proto::ModerationEntry {
                    verb: proto::ModerationVerb::ForceMute as i32,
                    mute_scope: proto::MuteScope::Audience as i32,
                    enforced: true,
                    ..proto::ModerationEntry::default()
                },
            )),
            ..proto::RoomEvent::default()
        },
    ];

    let client = serve(stub).await;
    let mut stream = client
        .room("live")
        .events(EventQuery::default())
        .await
        .expect("opening the stream");

    let first = stream
        .next()
        .await
        .expect("reading")
        .expect("an event arrives");
    assert_eq!(first.sequence, 7);
    match first.body {
        Body::Joined(joined) => {
            assert_eq!(joined.participant.identity.username, "carol");
            assert!(joined.first_device);
        }
        other => panic!("expected a join, got {other:?}"),
    }

    let second = stream
        .next()
        .await
        .expect("reading")
        .expect("a second event arrives");
    match second.body {
        Body::Moderation(entry) => {
            assert_eq!(entry.mute_scope, MuteScope::Audience);
            assert!(entry.enforced);
        }
        other => panic!("expected a moderation entry, got {other:?}"),
    }

    assert!(
        stream.next().await.expect("reading").is_none(),
        "the stream should end after its last event"
    );
}

/// Sequence numbers are per room, so there is nothing to resume from on a tenant-wide
/// stream. Refusing locally says so; sending it would earn an `InvalidArgument` that reads
/// like a server problem.
#[tokio::test]
async fn a_tenant_wide_stream_cannot_resume() {
    let client = serve(Stub::default()).await;

    let refused = client
        .events(EventQuery {
            after: 12,
            ..EventQuery::default()
        })
        .await;
    assert!(
        matches!(refused, Err(Error::Request(_))),
        "a tenant-wide stream accepted a cursor"
    );

    client
        .events(EventQuery::default())
        .await
        .expect("a tenant-wide stream without a cursor opens");
}

#[tokio::test]
async fn minting_derives_the_device_from_the_thumbprint() {
    let client = serve(Stub::default()).await;

    let token = client
        .mint_token(TokenRequest {
            username: "bob".into(),
            device_thumbprint: "thumb".into(),
            room: "live".into(),
            publish: Publish::CAMERA,
            preset: Some(Preset::Moderator),
            lifetime: Duration::from_mins(5),
            ..TokenRequest::default()
        })
        .await
        .expect("minting");

    assert_eq!(token.identity.username, "bob");
    assert!(
        !token.identity.device.is_empty(),
        "the device was not returned, so a backend cannot record what it handed out"
    );

    let keys = client
        .verification_keys()
        .await
        .expect("reading verification keys");
    assert_eq!(keys.binding, moqom_sdk::Binding::Bearer);
    assert_eq!(keys.keys.len(), 1);
}

#[test]
fn presets_expand_the_same_way_the_server_does() {
    assert_eq!(Preset::None.expand(), Capability::empty());
    assert_eq!(Preset::Moderator.expand(), Capability::CHAT);
    assert_eq!(
        Preset::SuperModerator.expand(),
        Capability::CHAT | Capability::STAGE
    );
    assert_eq!(Preset::Owner.expand(), Capability::EVERYTHING);
}

#[test]
fn role_summarises_grants() {
    use moqom_sdk::Role;

    assert_eq!(
        Grants {
            is_owner: true,
            ..Grants::default()
        }
        .role(),
        Role::Owner
    );
    assert_eq!(
        Grants {
            publish: Publish::CAMERA,
            moderation: Capability::FORCE_MUTE,
            is_owner: false,
        }
        .role(),
        Role::Cohost
    );
    assert_eq!(Grants::guest().role(), Role::Guest);
    assert_eq!(
        Grants {
            moderation: Capability::CHAT,
            ..Grants::default()
        }
        .role(),
        Role::Moderator
    );
    assert_eq!(Grants::viewer().role(), Role::Viewer);
}
