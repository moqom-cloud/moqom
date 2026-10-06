package mls

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"fmt"
)

// The two MoqomKit encodings the service reads: a key package, to check whose
// it is, and the header of a commit, to check which group and epoch it is for.
// Both are RFC 9420's TLS presentation language (§2.1) over MoqomKit's own
// structures; see MLSGroup.swift. Nothing else is parsed, because nothing else
// is the service's business.

const (
	protocolVersion = 0x0001
	cipherSuite     = 0x0001 // MLS_128_DHKEMX25519_AES128GCM_SHA256_Ed25519

	// Bounds on what a device may store, generous for a 200-byte package.
	maxKeyPackageBytes = 1 << 10
	maxOneTimePackages = 16
)

type reader struct {
	data []byte
	err  error
}

func (r *reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || len(r.data) < n {
		r.err = fmt.Errorf("%w: truncated", ErrInvalid)
		return nil
	}
	out := r.data[:n]
	r.data = r.data[n:]
	return out
}

func (r *reader) uint16() uint16 {
	b := r.take(2)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint16(b)
}

func (r *reader) uint32() uint32 {
	b := r.take(4)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint32(b)
}

func (r *reader) uint64() uint64 {
	b := r.take(8)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint64(b)
}

// length reads RFC 9420 §2.1.2's variable-length size, refusing a value that
// was not written in its shortest form, as the RFC requires.
func (r *reader) length() int {
	first := r.take(1)
	if first == nil {
		return 0
	}
	switch first[0] >> 6 {
	case 0:
		return int(first[0])
	case 1:
		rest := r.take(1)
		if rest == nil {
			return 0
		}
		value := int(first[0]&0x3f)<<8 | int(rest[0])
		if value < 1<<6 {
			r.err = fmt.Errorf("%w: length not minimally encoded", ErrInvalid)
		}
		return value
	case 2:
		rest := r.take(3)
		if rest == nil {
			return 0
		}
		value := int(first[0]&0x3f)<<24 | int(rest[0])<<16 | int(rest[1])<<8 | int(rest[2])
		if value < 1<<14 {
			r.err = fmt.Errorf("%w: length not minimally encoded", ErrInvalid)
		}
		return value
	default:
		r.err = fmt.Errorf("%w: reserved length prefix", ErrInvalid)
		return 0
	}
}

func (r *reader) vector() []byte { return r.take(r.length()) }

func writeVector(buffer *bytes.Buffer, data []byte) {
	switch n := len(data); {
	case n < 1<<6:
		buffer.WriteByte(byte(n))
	case n < 1<<14:
		buffer.Write([]byte{byte(n>>8) | 0x40, byte(n)})
	default:
		buffer.Write([]byte{byte(n>>24) | 0x80, byte(n >> 16), byte(n >> 8), byte(n)})
	}
	buffer.Write(data)
}

// KeyPackage is the part of a MoqomKit key package the service checks.
type KeyPackage struct {
	Identity      string
	SignatureKey  []byte
	InitKey       []byte
	EncryptionKey []byte
}

// ParseKeyPackage reads a key package and verifies it is signed by the key it
// carries, so a malformed or unsigned blob is refused here rather than handed
// to a committer.
func ParseKeyPackage(data []byte) (KeyPackage, error) {
	if len(data) > maxKeyPackageBytes {
		return KeyPackage{}, fmt.Errorf("%w: key package is %d bytes", ErrInvalid, len(data))
	}
	r := &reader{data: data}
	version, suite := r.uint16(), r.uint16()
	identity := r.vector()
	signatureKey := r.vector()
	initKey := r.vector()
	encryptionKey := r.vector()
	signed := len(data) - len(r.data)
	signature := r.vector()
	if r.err != nil {
		return KeyPackage{}, r.err
	}
	if len(r.data) != 0 {
		return KeyPackage{}, fmt.Errorf("%w: trailing bytes after key package", ErrInvalid)
	}
	if version != protocolVersion || suite != cipherSuite {
		return KeyPackage{}, fmt.Errorf("%w: version %#x suite %#x", ErrInvalid, version, suite)
	}
	if len(signatureKey) != ed25519.PublicKeySize || len(initKey) != 32 || len(encryptionKey) != 32 {
		return KeyPackage{}, fmt.Errorf("%w: key lengths", ErrInvalid)
	}
	if bytes.Equal(initKey, encryptionKey) {
		return KeyPackage{}, fmt.Errorf("%w: init key reused as encryption key", ErrInvalid)
	}
	if !ed25519.Verify(signatureKey, signContent("KeyPackageTBS", data[:signed]), signature) {
		return KeyPackage{}, fmt.Errorf("%w: key package signature", ErrInvalid)
	}
	return KeyPackage{
		Identity:      string(identity),
		SignatureKey:  signatureKey,
		InitKey:       initKey,
		EncryptionKey: encryptionKey,
	}, nil
}

// signContent is RFC 9420 §5.1.2's SignContent.
func signContent(label string, content []byte) []byte {
	var buffer bytes.Buffer
	writeVector(&buffer, []byte("MLS 1.0 "+label))
	writeVector(&buffer, content)
	return buffer.Bytes()
}

// CommitHeader is which group and epoch a commit says it is for.
type CommitHeader struct {
	GroupID []byte
	Epoch   uint64
	Sender  uint32
}

// ParseCommitHeader reads the start of a MoqomKit commit message. The rest --
// the signature, the sealed secrets, the confirmation tag -- is the members'
// to verify.
func ParseCommitHeader(data []byte) (CommitHeader, error) {
	r := &reader{data: data}
	header := CommitHeader{GroupID: r.vector(), Epoch: r.uint64(), Sender: r.uint32()}
	r.vector() // commit body
	r.vector() // signature
	r.vector() // confirmation tag
	if r.err != nil {
		return CommitHeader{}, r.err
	}
	if len(r.data) != 0 {
		return CommitHeader{}, fmt.Errorf("%w: trailing bytes after commit", ErrInvalid)
	}
	return header, nil
}
