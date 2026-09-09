package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

const (
	// Magic is the first two header bytes, "AL".
	Magic uint16 = 0x414C
	// Version is the only wire format version understood.
	Version uint8 = 1
	// HeaderLen is the fixed header size in bytes.
	HeaderLen = 18
	// MaxChunks is the u16 ceiling on the number of source chunks.
	MaxChunks = 0xFFFF
	// MaxFrameText is longer than any frame that fits QR version 40 (3420
	// characters); longer strings are rejected before decoding.
	MaxFrameText = 4096
)

// Type is the frame type byte.
type Type uint8

// Frame types.
const (
	TypeManifest Type = 0
	TypeData     Type = 1
	TypeFountain Type = 2
)

func (t Type) String() string {
	switch t {
	case TypeManifest:
		return "MANIFEST"
	case TypeData:
		return "DATA"
	case TypeFountain:
		return "FOUNTAIN"
	}
	return fmt.Sprintf("type(%d)", uint8(t))
}

// Frame parsing failures. Each is wrapped with detail; test with errors.Is.
var (
	ErrShort   = errors.New("short header")
	ErrMagic   = errors.New("bad magic")
	ErrVersion = errors.New("unsupported version")
	ErrType    = errors.New("unknown frame type")
	ErrLength  = errors.New("length mismatch")
	ErrCRC     = errors.New("crc mismatch")
	ErrTooLong = errors.New("frame text too long")
)

// Frame is one decoded frame. Payload is a MANIFEST JSON document, a chunk of
// the gzip blob, or (Phase 4) a fountain packet.
type Frame struct {
	Type    Type
	Session uint32
	Seq     uint16
	Total   uint16
	Payload []byte
}

// Pack serialises the frame: 18-byte big-endian header, then the payload.
func (f Frame) Pack() []byte {
	out := make([]byte, HeaderLen+len(f.Payload))
	binary.BigEndian.PutUint16(out[0:], Magic)
	out[2] = Version
	out[3] = byte(f.Type)
	binary.BigEndian.PutUint32(out[4:], f.Session)
	binary.BigEndian.PutUint16(out[8:], f.Seq)
	binary.BigEndian.PutUint16(out[10:], f.Total)
	binary.BigEndian.PutUint16(out[12:], uint16(len(f.Payload)))
	binary.BigEndian.PutUint32(out[14:], crc32.ChecksumIEEE(f.Payload))
	copy(out[HeaderLen:], f.Payload)
	return out
}

// Text is the frame as it travels: base45, ready for QR alphanumeric mode.
func (f Frame) Text() string {
	return Base45Encode(f.Pack())
}

// Parse validates and decodes a packed frame. The payload is copied.
func Parse(raw []byte) (Frame, error) {
	if len(raw) < HeaderLen {
		return Frame{}, fmt.Errorf("%w: %d bytes", ErrShort, len(raw))
	}
	if m := binary.BigEndian.Uint16(raw[0:]); m != Magic {
		return Frame{}, fmt.Errorf("%w: 0x%04X", ErrMagic, m)
	}
	if raw[2] != Version {
		return Frame{}, fmt.Errorf("%w: %d", ErrVersion, raw[2])
	}
	t := Type(raw[3])
	if t != TypeManifest && t != TypeData && t != TypeFountain {
		return Frame{}, fmt.Errorf("%w: %d", ErrType, raw[3])
	}
	length := int(binary.BigEndian.Uint16(raw[12:]))
	if len(raw) != HeaderLen+length {
		return Frame{}, fmt.Errorf("%w: header says %d, got %d", ErrLength, length, len(raw)-HeaderLen)
	}
	payload := raw[HeaderLen:]
	if got, want := crc32.ChecksumIEEE(payload), binary.BigEndian.Uint32(raw[14:]); got != want {
		return Frame{}, fmt.Errorf("%w: computed %08x, header %08x", ErrCRC, got, want)
	}
	return Frame{
		Type:    t,
		Session: binary.BigEndian.Uint32(raw[4:]),
		Seq:     binary.BigEndian.Uint16(raw[8:]),
		Total:   binary.BigEndian.Uint16(raw[10:]),
		Payload: append([]byte(nil), payload...),
	}, nil
}

// ParseText decodes a base45 frame string as relayed by a scanner.
func ParseText(s string) (Frame, error) {
	if len(s) > MaxFrameText {
		return Frame{}, fmt.Errorf("%w: %d characters", ErrTooLong, len(s))
	}
	raw, err := Base45Decode(s)
	if err != nil {
		return Frame{}, err
	}
	return Parse(raw)
}
