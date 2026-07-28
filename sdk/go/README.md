# moqom-go

The server-side SDK for [MOQOM](https://moqom.cloud). For a backend, not a
participant: it mints the tokens your users join with, creates and closes rooms,
moderates them, and follows what happens in them.

It does not carry media. Nothing here speaks MOQT, and a Go process that wants to
receive frames — a recorder, a classifier — wants a MOQT subscriber instead. The
split is deliberate: media needs a QUIC stack, a codec, and a scheduler on the hot
path, and making every backend link all three to change somebody's grants would be
a poor trade.

```sh
go get github.com/moqom-cloud/moqom-go
```

## The call you will make most

Your users authenticate however they already do. You then ask for a token naming
that user, that device, that room, and exactly the rights they should have — MOQOM
never sees your password database.

```go
client, err := moqom.Dial("api.moqom.cloud:443",
    moqom.WithCredential(os.Getenv("MOQOM_SERVICE_KEY")),
    moqom.WithTenant("acme"))
if err != nil {
    return err
}
defer client.Close()

token, err := client.MintToken(ctx, moqom.TokenRequest{
    Username:         user.Name,
    DeviceThumbprint: request.Thumbprint, // from the client's device key
    Room:             "live/42",
    Publish:          moqom.PublishAudio | moqom.PublishVideo,
    Preset:           moqom.PresetNone,
})
```

## Moderation

Attribute the action to the human who asked for it. The service account comes from
the credentials on the connection and cannot be set per call, so an audit trail is
not something a caller can forge.

```go
room := client.Room("live/42")

_, err = room.Kick(ctx, moqom.KickRequest{
    Target: moqom.User("carol"),
    Code:   moqom.CodeHarassment,
    Actor:  moqom.Actor{OnBehalfOf: "jane", Reason: "repeated slurs in chat"},
})
```

Every call returns the moderation entry it produced, and its `Enforced` field is
the honest answer to "did that work": false means the record exists and a relay
holding an affected session has not confirmed the directive yet. Surface it. A
moderator told a kick succeeded who then watches the person keep talking will not
trust the tool again.

## Following a room

`Events` is your substitute for being in the room. Delivery is at-least-once and
ordered per room, and every event carries a sequence number contiguous within its
room — so persist it. A consumer that resumes from the last sequence it handled can
tell it *missed* something, which a timestamp cannot.

```go
stream, err := room.Events(ctx, moqom.EventQuery{After: cursor})
if err != nil {
    return err
}
for {
    event, err := stream.Recv()
    if err != nil {
        return err // reconnect from your cursor, not from the present
    }
    cursor = event.Sequence
}
```

## Identity

There are no numeric user IDs and nowhere to put one. A username is a string you
already have, and a device is derived from the device key's JWK thumbprint — so a
device ban and the thumbprint you passed to `MintToken` are the same fact. That is
what makes migrating from a platform with its own UID space a matter of pointing at
a new server rather than building a mapping table.

## Retries

Every mutating call carries an idempotency key, generated here when you do not
supply one, and a retry reuses it. A queue that redelivers "ban for 24 hours"
therefore does not extend the ban.

Supply your own key in `Actor` when the retry might come from a different process
than the first attempt — a key generated here cannot outlive it.

## Errors

Compare with `errors.Is` rather than importing gRPC to tell "no such room" from
"not allowed":

```go
if _, err := room.Kick(ctx, request); errors.Is(err, moqom.ErrDenied) {
    // the moderator lacks standing over this target
}
```

`*moqom.Error` names the operation, keeps the status code, and reports
`Retryable()` for a caller deciding whether to requeue.

## Licence

Apache-2.0.
