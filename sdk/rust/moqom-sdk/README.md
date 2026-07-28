# moqom-sdk

The server-side SDK for [MOQOM](https://moqom.cloud). For a backend, not a
participant: it mints the tokens your users join with, creates and closes rooms,
moderates them, and follows what happens in them.

Nothing here speaks MOQT, and that is the point — there is no QUIC in this crate's
dependency graph. A Rust process that wants to *receive frames* is a MOQT
subscriber and is a different dependency.

```toml
[dependencies]
moqom-sdk = "0.1"
```

## The call you will make most

Your users authenticate however they already do. You then ask for a token naming
that user, that device, that room, and exactly the rights they should have — MOQOM
never sees your password database.

```rust,no_run
use moqom_sdk::{Client, Preset, Publish, TokenRequest};

# async fn example(credential: String, thumbprint: String) -> Result<(), moqom_sdk::Error> {
let client = Client::builder("https://api.moqom.cloud")
    .tenant("acme")
    .credential(credential)
    .connect()
    .await?;

let token = client
    .mint_token(TokenRequest {
        username: "bob".into(),
        device_thumbprint: thumbprint, // from the client's device key
        room: "live/42".into(),
        publish: Publish::CAMERA,
        preset: Some(Preset::None),
        ..Default::default()
    })
    .await?;
# Ok(())
# }
```

## Moderation

Attribute the action to the human who asked for it. The service account comes from
the credentials on the connection and cannot be set per call, so an audit trail is
not something a caller can forge.

```rust,no_run
use moqom_sdk::{Actor, Client, Code, KickRequest, target};

# async fn example(client: Client) -> Result<(), moqom_sdk::Error> {
let room = client.room("live/42");

let entry = room
    .kick(KickRequest {
        target: target("carol"),
        code: Code::Harassment,
        actor: Actor::on_behalf_of("jane").because("repeated slurs in chat"),
    })
    .await?;

// The honest answer to "did that work".
if !entry.enforced {
    // The record exists; a relay holding an affected session has not confirmed yet.
}
# Ok(())
# }
```

Surface `enforced`. A moderator told a kick succeeded who then watches the person
keep talking will not trust the tool again.

## Following a room

Delivery is at-least-once and ordered per room, and every event carries a sequence
number contiguous within its room — so persist it. A consumer that resumes from the
last sequence it handled can tell it *missed* something, which a timestamp cannot.

```rust,no_run
use moqom_sdk::{Body, Client, EventQuery};

# async fn example(client: Client, mut cursor: u64) -> Result<(), moqom_sdk::Error> {
let room = client.room("live/42");
let mut events = room.events(EventQuery { after: cursor, ..Default::default() }).await?;

while let Some(event) = events.next().await? {
    cursor = event.sequence;
    if let Body::Moderation(entry) = event.body {
        // Including actions taken by another console or a safety classifier.
        let _ = entry;
    }
}
# Ok(())
# }
```

## Identity

There are no numeric user IDs and nowhere to put one. A username is a string you
already have, and a device is derived from the device key's JWK thumbprint — so a
device ban and the thumbprint you passed to `mint_token` are the same fact.

## Retries

Every mutating call carries an idempotency key, generated here when you do not
supply one, and a retry reuses it. A queue that redelivers "ban for 24 hours"
therefore does not extend the ban. Set `Actor::keyed` when the retry might come
from a different process than the first attempt.

## Grants

`Capability` is a set, never a level. An actor may act on a target only when the
actor's set strictly contains the target's, so two moderators with identical powers
cannot act on each other — the property every level scheme gets wrong. The bit
positions are protocol: a token carries them, the relay reads them, and the Go and
Swift SDKs use the same ones.

## Licence

Apache-2.0.
