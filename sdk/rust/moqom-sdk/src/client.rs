//! The connection, and the handle for one room.

use std::time::Duration;

use tonic::metadata::MetadataValue;
use tonic::service::Interceptor;
use tonic::service::interceptor::InterceptedService;
use tonic::transport::{Channel, ClientTlsConfig, Endpoint};

use crate::error::{Error, Result, retryable};
use crate::proto;
use crate::types::Actor;

/// A service that carries the credential on every call.
pub(crate) type Authenticated = InterceptedService<Channel, Bearer>;

pub(crate) type RoomsClient = proto::rooms_client::RoomsClient<Authenticated>;
pub(crate) type ModerationClient = proto::moderation_client::ModerationClient<Authenticated>;
pub(crate) type TokensClient = proto::tokens_client::TokensClient<Authenticated>;
pub(crate) type EventsClient = proto::events_client::EventsClient<Authenticated>;

/// A connection to a MOQOM control plane.
///
/// Cheap to clone -- the underlying channel is shared -- and meant to be long-lived:
/// building one per request throws away the handshake that makes the calls cheap.
#[derive(Clone, Debug)]
pub struct Client {
    pub(crate) tenant: String,
    pub(crate) actor: Actor,
    pub(crate) retry: Retry,
    pub(crate) rooms: RoomsClient,
    pub(crate) moderation: ModerationClient,
    pub(crate) tokens: TokensClient,
    pub(crate) events: EventsClient,
}

/// How transient failures are retried.
#[derive(Clone, Copy, Debug)]
pub(crate) struct Retry {
    pub(crate) attempts: u32,
    pub(crate) base: Duration,
}

impl Default for Retry {
    fn default() -> Self {
        Self {
            attempts: 3,
            base: Duration::from_millis(100),
        }
    }
}

/// Builds a [`Client`].
#[derive(Debug, Default)]
pub struct ClientBuilder {
    endpoint: String,
    tenant: String,
    credential: String,
    actor: Actor,
    retry: Retry,
    tls: Option<ClientTlsConfig>,
    allow_plaintext: bool,
}

impl Client {
    /// Start configuring a client.
    ///
    /// The endpoint is a URI: `https://api.moqom.cloud` in production, `http://...` only
    /// for a sidecar on loopback -- see [`ClientBuilder::allow_plaintext`].
    #[must_use]
    pub fn builder(endpoint: impl Into<String>) -> ClientBuilder {
        ClientBuilder {
            endpoint: endpoint.into(),
            ..ClientBuilder::default()
        }
    }

    /// The tenant this client acts on.
    #[must_use]
    pub fn tenant(&self) -> &str {
        &self.tenant
    }

    /// A handle for one room.
    ///
    /// No I/O: it is a name plus this client, and every method on it is a call.
    #[must_use]
    pub fn room(&self, name: impl Into<String>) -> Room<'_> {
        Room {
            client: self,
            name: name.into(),
        }
    }

    /// The wire actor for a call, generating an idempotency key when the caller did not
    /// supply one.
    ///
    /// One key per logical call rather than per attempt: that is the whole point, and
    /// generating it inside the retry loop would defeat it silently.
    pub(crate) fn actor(&self, actor: &Actor) -> proto::ActorContext {
        let on_behalf_of = if actor.on_behalf_of.is_empty() {
            self.actor.on_behalf_of.clone()
        } else {
            actor.on_behalf_of.clone()
        };
        let reason = if actor.reason.is_empty() {
            self.actor.reason.clone()
        } else {
            actor.reason.clone()
        };
        let idempotency_key = if actor.idempotency_key.is_empty() {
            random_key()
        } else {
            actor.idempotency_key.clone()
        };

        proto::ActorContext {
            on_behalf_of,
            reason,
            idempotency_key,
        }
    }
}

/// Addresses one room.
#[derive(Debug)]
pub struct Room<'a> {
    pub(crate) client: &'a Client,
    pub(crate) name: String,
}

impl Room<'_> {
    /// The room's identifier within the tenant.
    #[must_use]
    pub fn name(&self) -> &str {
        &self.name
    }
}

impl ClientBuilder {
    /// Which tenant this client acts on. Required.
    #[must_use]
    pub fn tenant(mut self, tenant: impl Into<String>) -> Self {
        self.tenant = tenant.into();
        self
    }

    /// The service account credential. Required.
    ///
    /// Sent as a bearer token, and only over TLS unless [`Self::allow_plaintext`] says
    /// otherwise.
    #[must_use]
    pub fn credential(mut self, credential: impl Into<String>) -> Self {
        self.credential = credential.into();
        self
    }

    /// Default attribution for every call.
    ///
    /// Useful for a backend that is only ever one thing -- a safety pipeline setting its
    /// reason once rather than at every call site. A per-call actor still wins.
    #[must_use]
    pub fn actor(mut self, actor: Actor) -> Self {
        self.actor = actor;
        self
    }

    /// How many attempts a call gets, and the first backoff.
    ///
    /// One attempt disables retrying. Only failures that cannot have applied a change are
    /// retried, and every attempt reuses the idempotency key.
    #[must_use]
    pub fn retry(mut self, attempts: u32, base: Duration) -> Self {
        self.retry = Retry {
            attempts: attempts.max(1),
            base,
        };
        self
    }

    /// Replace the TLS configuration, for a private certificate authority or pinning.
    ///
    /// The default is the system roots.
    #[must_use]
    pub fn tls(mut self, config: ClientTlsConfig) -> Self {
        self.tls = Some(config);
        self
    }

    /// Permit an `http://` endpoint.
    ///
    /// For a test, a sidecar on loopback, or a mesh terminating TLS elsewhere. Worth
    /// pausing over: on any network a third party can see, this hands out a bearer token
    /// that grants everything the service account can do. Without this, an `http://`
    /// endpoint is refused rather than quietly accepted.
    #[must_use]
    pub fn allow_plaintext(mut self) -> Self {
        self.allow_plaintext = true;
        self
    }

    /// Connect.
    ///
    /// # Errors
    ///
    /// Fails if the configuration is incomplete, if the endpoint is not a URI, or if the
    /// connection cannot be established.
    pub async fn connect(self) -> Result<Client> {
        // Configuration is checked before dialling, not after. Otherwise a missing tenant
        // costs a connection timeout to discover and reports itself as a network problem,
        // which sends the reader looking at DNS for a mistake that is in their own code.
        self.check()?;

        let channel = self.channel()?.connect().await?;
        self.build(channel)
    }

    /// Use a channel the caller already has.
    ///
    /// For a process that multiplexes several APIs over one connection, or a test over an
    /// in-memory transport. The credential is still attached to every call.
    ///
    /// # Errors
    ///
    /// Fails if the tenant or credential is missing.
    pub fn with_channel(self, channel: Channel) -> Result<Client> {
        self.build(channel)
    }

    fn channel(&self) -> Result<Endpoint> {
        if self.endpoint.is_empty() {
            return Err(Error::request("moqom: the endpoint cannot be empty"));
        }

        let mut endpoint = Endpoint::from_shared(self.endpoint.clone()).map_err(|e| {
            Error::request(format!(
                "moqom: {} is not a usable endpoint: {e}",
                self.endpoint
            ))
        })?;

        let plaintext = !self.endpoint.starts_with("https://");
        if plaintext && !self.allow_plaintext {
            return Err(Error::request(format!(
                "moqom: {} would send the service account credential in the clear; \
                 use https, or allow_plaintext() if this is loopback or a mesh",
                self.endpoint
            )));
        }
        if !plaintext {
            let tls = self
                .tls
                .clone()
                .unwrap_or_else(|| ClientTlsConfig::new().with_native_roots());
            endpoint = endpoint.tls_config(tls)?;
        }

        // Relays and long-lived event streams sit idle for hours in a quiet region.
        // Without keepalives a middlebox drops the connection, and the consumer does not
        // find out until it has an event to miss.
        Ok(endpoint
            .http2_keep_alive_interval(Duration::from_secs(30))
            .keep_alive_timeout(Duration::from_secs(10))
            .keep_alive_while_idle(true))
    }

    fn check(&self) -> Result<()> {
        if self.tenant.is_empty() {
            return Err(Error::request("moqom: a tenant is required"));
        }
        if self.credential.is_empty() {
            return Err(Error::request("moqom: a credential is required"));
        }
        Ok(())
    }

    fn build(self, channel: Channel) -> Result<Client> {
        self.check()?;

        // One channel behind all four services, so they share a connection and its
        // keepalives rather than opening one apiece.
        let bearer = Bearer::new(&self.credential)?;
        let rooms = InterceptedService::new(channel.clone(), bearer.clone());
        let moderation = InterceptedService::new(channel.clone(), bearer.clone());
        let tokens = InterceptedService::new(channel.clone(), bearer.clone());
        let events = InterceptedService::new(channel, bearer);

        Ok(Client {
            tenant: self.tenant,
            actor: self.actor,
            retry: self.retry,
            rooms: proto::rooms_client::RoomsClient::new(rooms),
            moderation: proto::moderation_client::ModerationClient::new(moderation),
            tokens: proto::tokens_client::TokensClient::new(tokens),
            events: proto::events_client::EventsClient::new(events),
        })
    }
}

/// Attaches the service account credential to every call.
#[derive(Clone)]
pub struct Bearer {
    header: MetadataValue<tonic::metadata::Ascii>,
}

impl std::fmt::Debug for Bearer {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        // Never the value. A client is a reasonable thing to log at debug level, and a
        // credential in a log is a credential in a log aggregator.
        f.write_str("Bearer(redacted)")
    }
}

impl Bearer {
    fn new(credential: &str) -> Result<Self> {
        let header: MetadataValue<_> = format!("Bearer {credential}")
            .parse()
            .map_err(|_| Error::request("moqom: the credential is not a valid header value"))?;
        Ok(Self { header })
    }
}

impl Interceptor for Bearer {
    fn call(
        &mut self,
        mut request: tonic::Request<()>,
    ) -> std::result::Result<tonic::Request<()>, tonic::Status> {
        request
            .metadata_mut()
            .insert("authorization", self.header.clone());
        Ok(request)
    }
}

/// Runs a call, retrying the failures that cannot have applied a change.
///
/// The unit of retry is the whole call including its idempotency key, so a retry after a
/// timeout on a request the server did process returns that first result rather than
/// acting twice.
pub(crate) async fn call<T, F, Fut>(
    retry: Retry,
    operation: &'static str,
    mut attempt: F,
) -> Result<T>
where
    F: FnMut() -> Fut,
    Fut: Future<Output = std::result::Result<T, tonic::Status>>,
{
    let mut delay = retry.base;
    let mut last = None;

    for number in 0..retry.attempts {
        if number > 0 {
            tokio::time::sleep(delay).await;
            delay *= 2;
        }

        match attempt().await {
            Ok(value) => return Ok(value),
            Err(status) if retryable(&status) => last = Some(status),
            Err(status) => return Err(Error::from_status(operation, &status)),
        }
    }

    Err(Error::from_status(
        operation,
        &last.unwrap_or_else(|| tonic::Status::unavailable("no attempt was made")),
    ))
}

/// A random idempotency key.
fn random_key() -> String {
    let mut raw = [0u8; 16];
    if getrandom::fill(&mut raw).is_err() {
        // Only reachable if the system entropy source is broken, at which point a key
        // colliding is the least of the caller's problems. A monotonic fallback keeps the
        // call from failing outright.
        let nanos = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map_or(0, |since| since.as_nanos());
        return format!("t-{nanos}");
    }

    let mut key = String::with_capacity(32);
    for byte in raw {
        use std::fmt::Write;
        let _ = write!(key, "{byte:02x}");
    }
    key
}
