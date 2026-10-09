package apple

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// The CBOR (RFC 8949) that App Attest objects are written in, and no more.
//
// An attestation is a map of text keys to a text string, a byte string and a
// map holding an array of byte strings; an assertion is a map of two byte
// strings. So this reads unsigned integers, byte and text strings, arrays and
// maps with text keys, all of definite length, and refuses everything else:
// tags, floats, negative numbers, indefinite lengths, duplicate keys and bytes
// left over. A decoder that accepted more would be more code that an
// unauthenticated request reaches.

var errCBOR = errors.New("malformed CBOR")

// maxCBORDepth is deeper than any App Attest object nests.
const maxCBORDepth = 4

type cborReader struct {
	data []byte
	at   int
}

// decodeCBOR reads one item that must be the whole of data.
func decodeCBOR(data []byte) (any, error) {
	r := &cborReader{data: data}
	value, err := r.item(0)
	if err != nil {
		return nil, err
	}
	if r.at != len(data) {
		return nil, fmt.Errorf("%w: %d bytes after the item", errCBOR, len(data)-r.at)
	}
	return value, nil
}

func (r *cborReader) head() (major byte, argument uint64, err error) {
	if r.at >= len(r.data) {
		return 0, 0, fmt.Errorf("%w: truncated", errCBOR)
	}
	initial := r.data[r.at]
	r.at++
	major, info := initial>>5, initial&0x1f
	switch {
	case info < 24:
		return major, uint64(info), nil
	case info <= 27:
		size := 1 << (info - 24)
		if len(r.data)-r.at < size {
			return 0, 0, fmt.Errorf("%w: truncated", errCBOR)
		}
		var buffer [8]byte
		copy(buffer[8-size:], r.data[r.at:r.at+size])
		r.at += size
		return major, binary.BigEndian.Uint64(buffer[:]), nil
	default:
		return 0, 0, fmt.Errorf("%w: additional information %d is not supported", errCBOR, info)
	}
}

func (r *cborReader) bytes(n uint64) ([]byte, error) {
	if n > uint64(len(r.data)-r.at) {
		return nil, fmt.Errorf("%w: truncated", errCBOR)
	}
	out := r.data[r.at : r.at+int(n)]
	r.at += int(n)
	return out, nil
}

func (r *cborReader) item(depth int) (any, error) {
	if depth > maxCBORDepth {
		return nil, fmt.Errorf("%w: nested too deeply", errCBOR)
	}
	major, argument, err := r.head()
	if err != nil {
		return nil, err
	}
	switch major {
	case 0:
		return argument, nil
	case 2:
		return r.bytes(argument)
	case 3:
		text, err := r.bytes(argument)
		return string(text), err
	case 4:
		// Each element takes at least a byte, which bounds the allocation.
		if argument > uint64(len(r.data)-r.at) {
			return nil, fmt.Errorf("%w: truncated", errCBOR)
		}
		items := make([]any, 0, argument)
		for range argument {
			value, err := r.item(depth + 1)
			if err != nil {
				return nil, err
			}
			items = append(items, value)
		}
		return items, nil
	case 5:
		if argument > uint64(len(r.data)-r.at)/2 {
			return nil, fmt.Errorf("%w: truncated", errCBOR)
		}
		entries := make(map[string]any, argument)
		for range argument {
			key, err := r.item(depth + 1)
			if err != nil {
				return nil, err
			}
			name, ok := key.(string)
			if !ok {
				return nil, fmt.Errorf("%w: a map key that is not text", errCBOR)
			}
			if _, seen := entries[name]; seen {
				return nil, fmt.Errorf("%w: duplicate key %q", errCBOR, name)
			}
			value, err := r.item(depth + 1)
			if err != nil {
				return nil, err
			}
			entries[name] = value
		}
		return entries, nil
	default:
		return nil, fmt.Errorf("%w: major type %d is not supported", errCBOR, major)
	}
}
