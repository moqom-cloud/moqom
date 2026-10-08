package token

import (
	"errors"
	"fmt"
	"strings"
)

// The room control log is signed by the control plane with the same ML-DSA-65
// keys it signs tokens with, and clients apply only entries that verify against
// the published key set. A relay forwarding the log can drop an entry, which the
// log's sequence numbers surface; it cannot write one, alter one, or move one
// from another room.
//
// MOQOM's protocol rather than moqom-token's, so it stays here and signs
// through the library's Signer.SignMessage, whose domain separation it owns.
//
// What is signed is roomLogContext ‖ 0 ‖ tenant ‖ 0 ‖ room ‖ 0 ‖ entry. The
// context keeps a log signature from ever verifying as a token's — a JWS signs
// base64url text, which never begins with it — and naming the room keeps an
// entry signed for one room from verifying in another. The result is carried as
// the MOQOM authority-signature object property (0x7F): one byte of key ID
// length, the key ID, then the signature. MoqomKit's RoomLogSignature is the
// other half of this and must agree byte for byte.
const roomLogContext = "moqom room log v1"

// RoomLogSignatureProperty is the object property code a signed entry carries.
const RoomLogSignatureProperty = 0x7F

// ErrRoomLogUnsignable is returned for a key that cannot sign log entries:
// a verify-only legacy key, or one whose ID will not fit the property.
var ErrRoomLogUnsignable = errors.New("token: key cannot sign the room log")

// SignRoomLog signs one encoded control-log entry for tenant and room, and
// returns the authority-signature property to publish beside it.
func (k Key) SignRoomLog(tenant, room string, entry []byte) ([]byte, error) {
	if k.signer == nil || len(k.ID) > 255 {
		return nil, ErrRoomLogUnsignable
	}
	message, err := roomLogMessage(tenant, room, entry)
	if err != nil {
		return nil, err
	}
	signature, err := k.signer.SignMessage(message)
	if err != nil {
		return nil, fmt.Errorf("token: signing a room log entry: %w", err)
	}
	property := make([]byte, 0, 1+len(k.ID)+len(signature))
	property = append(property, byte(len(k.ID)))
	property = append(property, k.ID...)
	return append(property, signature...), nil
}

// VerifyRoomLog reports whether property is a valid authority signature over
// entry in tenant's room, by one of keys. ML-DSA-65 keys only: the log was never
// signed with anything else, so a legacy Ed25519 key verifies no entry.
func VerifyRoomLog(keys []Key, tenant, room string, entry, property []byte) bool {
	if len(property) < 1 {
		return false
	}
	end := 1 + int(property[0])
	if len(property) <= end {
		return false
	}
	id := string(property[1:end])
	message, err := roomLogMessage(tenant, room, entry)
	if err != nil {
		return false
	}
	for _, key := range keys {
		if key.ID == id && key.Algorithm == AlgorithmMLDSA65 {
			return key.public.Verify(message, property[end:])
		}
	}
	return false
}

func roomLogMessage(tenant, room string, entry []byte) ([]byte, error) {
	if strings.ContainsRune(tenant, 0) || strings.ContainsRune(room, 0) {
		return nil, errors.New("token: a tenant or room name contains NUL")
	}
	message := make([]byte, 0, len(roomLogContext)+len(tenant)+len(room)+len(entry)+3)
	message = append(message, roomLogContext...)
	message = append(message, 0)
	message = append(message, tenant...)
	message = append(message, 0)
	message = append(message, room...)
	message = append(message, 0)
	return append(message, entry...), nil
}
