//! Minting the tokens a client joins with.

use std::time::Duration;

use crate::client::{Client, call};
use crate::convert::{grants_out, identity_in, preset_out, time_in};
use crate::error::{Error, Result};
use crate::grants::{Capability, Grants, Preset, Publish};
use crate::proto;
use crate::types::{Binding, Token, VerificationKey};

/// Asks for a token a client can join with.
///
/// This is the call a backend makes most, and the one that decides who anybody is. The
/// customer authenticates its user however it already does, then asks for a token naming
/// that user, that device, that room, and exactly the rights they should have. MOQOM never
/// sees the customer's password database.
#[derive(Debug, Clone, Default)]
pub struct TokenRequest {
    /// The account this token speaks for -- the customer's own identifier. MOQOM has no
    /// opinion about its shape beyond it being a string.
    pub username: String,

    /// The device public key's JWK thumbprint, base64url of the SHA-256.
    ///
    /// The client generates the key in its secure element and never exports it, so this is
    /// the only part of the device identity a backend handles. The device field of every
    /// identity for this session is derived from it, which is what makes a device ban and
    /// this value the same fact.
    pub device_thumbprint: String,

    /// Which room. Empty mints a token good for the tenant's discovery surface but no
    /// room, which is what a client needs before it has chosen one.
    pub room: String,

    /// What they may send. A grant is permission, never an action.
    pub publish: Publish,

    /// What they may moderate. Subject to the service account's own ceiling: a key cannot
    /// mint a token more powerful than itself, which is what keeps a leaked integration key
    /// from becoming an owner of every room.
    pub moderation: Capability,

    /// Replaces `moderation` with the preset's expansion.
    pub preset: Option<Preset>,

    /// Make this token's holder the room's owner: immune to moderation, and not demotable.
    /// Still bounded by the account's ceiling.
    pub owner: bool,

    /// How long the token is good for. Zero takes the server's default. Short on purpose --
    /// a token is the one credential that leaves the building, and until proof-of-possession
    /// lands a stolen one is replayable.
    pub lifetime: Duration,

    /// Join backstage rather than the room proper. The token names the backstage namespace,
    /// and going live later is a namespace change rather than a new token.
    pub backstage: bool,

    /// Attest that this user's age has been verified, for rooms whose policy demands it.
    /// The tenant performs the verification; this asserts it happened.
    pub age_verified: bool,
}

/// What the control plane publishes for verifying tokens.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct VerificationKeys {
    /// The keys, newest first.
    pub keys: Vec<VerificationKey>,

    /// What binding this deployment issues. A client that wants to refuse bearer tokens can
    /// find out here rather than by inspecting one.
    pub binding: Binding,
}

impl Client {
    /// Issue a client token.
    ///
    /// Not retried on an ambiguous failure, unlike everything else here, and that is
    /// deliberate: minting is cheap, has no idempotency key, and a second token is harmless
    /// where a second ban is not. Ask again.
    ///
    /// # Errors
    ///
    /// [`Error::Denied`] for a token more powerful than the service account,
    /// [`Error::Closed`] for a room that has ended, and [`Error::Denied`] for an age-gated
    /// room without `age_verified`.
    pub async fn mint_token(&self, request: TokenRequest) -> Result<Token> {
        if request.username.is_empty() {
            return Err(Error::request("moqom: minting needs a username"));
        }
        if request.device_thumbprint.is_empty() {
            return Err(Error::request("moqom: minting needs a device thumbprint"));
        }

        let wire = proto::MintClientTokenRequest {
            tenant: self.tenant.clone(),
            username: request.username,
            device_thumbprint: request.device_thumbprint,
            room: request.room,
            grants: Some(grants_out(Grants {
                publish: request.publish,
                moderation: request.moderation,
                is_owner: request.owner,
            })),
            preset: preset_out(request.preset),
            lifetime: crate::types::optional(request.lifetime),
            backstage: request.backstage,
            age_verified: request.age_verified,
        };

        let response = self
            .tokens
            .clone()
            .mint_client_token(wire)
            .await
            .map_err(|status| Error::from_status("mint_token", &status))?
            .into_inner();

        Ok(Token {
            token: response.token,
            expires_at: time_in(response.expires_at),
            identity: identity_in(response.identity),
            key_id: response.key_id,
        })
    }

    /// The public keys tokens can be verified with.
    ///
    /// Served rather than configured so a key can be rotated without a deploy: a new key is
    /// published, tokens start being signed with it, and the old one is withdrawn once every
    /// token it signed has expired. A caller that verifies tokens itself should refetch
    /// periodically rather than caching forever.
    ///
    /// # Errors
    ///
    /// Fails if the call does.
    pub async fn verification_keys(&self) -> Result<VerificationKeys> {
        let wire = proto::ListVerificationKeysRequest {
            tenant: self.tenant.clone(),
        };

        let response = call(self.retry, "verification_keys", || {
            let mut tokens = self.tokens.clone();
            let wire = wire.clone();
            async move { tokens.list_verification_keys(wire).await }
        })
        .await?
        .into_inner();

        Ok(VerificationKeys {
            keys: response
                .keys
                .into_iter()
                .map(|key| VerificationKey {
                    key_id: key.key_id,
                    algorithm: key.algorithm,
                    jwk: key.jwk,
                    not_before: time_in(key.not_before),
                    retires_at: time_in(key.retires_at),
                })
                .collect(),
            binding: match proto::TokenBinding::try_from(response.binding) {
                Ok(proto::TokenBinding::Bearer) => Binding::Bearer,
                Ok(proto::TokenBinding::Dpop) => Binding::DPoP,
                _ => Binding::Unknown,
            },
        })
    }
}
