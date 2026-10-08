package mls

import (
	"fmt"

	moqommls "github.com/moqom-cloud/moqom-security/go/mls"
)

// The two MoqomKit encodings the service reads: a key package, to check whose
// it is, and the header of a commit, to check which group and epoch it is for.
// moqom-mls decodes and verifies both; nothing else is parsed, because nothing
// else is the service's business.

// CipherSuite is an MLS cipher suite identifier, RFC 9420 §17.1.
type CipherSuite = moqommls.CipherSuite

// The suites moqom-mls implements; see its CipherSuite.
const (
	SuiteX25519 = moqommls.SuiteX25519
	SuiteXWing  = moqommls.SuiteXWing
	SuiteMLDSA  = moqommls.SuiteMLDSA
)

// FoundingSuite is the suite every new group is founded in.
const FoundingSuite = moqommls.FoundingSuite

const (
	// Bounds on what a device may store. A package is about 200 bytes in
	// SuiteX25519, about 2.5 KiB in SuiteXWing, whose two keys are 1,216 bytes
	// each, and about 7.5 KiB in SuiteMLDSA (two 1,184-byte keys, a 1,952-byte
	// ML-DSA key and a 3,309-byte signature); 16 KiB leaves room for a long
	// identity in any.
	maxKeyPackageBytes = 16 << 10
	maxOneTimePackages = 16
)

// KeyPackage is the part of a MoqomKit key package the service checks.
type KeyPackage struct {
	CipherSuite   CipherSuite
	Identity      string
	SignatureKey  []byte
	InitKey       []byte
	EncryptionKey []byte
}

// ParseKeyPackage reads a key package and verifies it is signed by the key it
// carries, so a malformed or unsigned blob is refused here rather than handed
// to a committer. A suite moqom-mls does not implement is refused.
func ParseKeyPackage(data []byte) (KeyPackage, error) {
	if len(data) > maxKeyPackageBytes {
		return KeyPackage{}, fmt.Errorf("%w: key package is %d bytes", ErrInvalid, len(data))
	}
	parsed, err := moqommls.ParseKeyPackage(data)
	if err != nil {
		return KeyPackage{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return KeyPackage{
		CipherSuite:   parsed.CipherSuite,
		Identity:      string(parsed.Identity),
		SignatureKey:  parsed.SignatureKey,
		InitKey:       parsed.InitKey,
		EncryptionKey: parsed.EncryptionKey,
	}, nil
}

// CommitHeader is which group and epoch a commit says it is for.
type CommitHeader struct {
	GroupID []byte
	Epoch   uint64
	Sender  uint32
}

// ParseCommitHeader reads a MoqomKit commit message's framing. The rest --
// the signature, the sealed secrets, the confirmation tag -- is the members'
// to verify.
func ParseCommitHeader(data []byte) (CommitHeader, error) {
	message, err := moqommls.DecodeCommitMessage(data)
	if err != nil {
		return CommitHeader{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return CommitHeader{GroupID: message.GroupID, Epoch: message.Epoch, Sender: message.Sender}, nil
}
