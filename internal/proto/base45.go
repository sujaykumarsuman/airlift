package proto

import (
	"errors"
	"fmt"
	"strings"
)

// Base45Alphabet is RFC 9285's alphabet, which is exactly QR's alphanumeric
// character set.
const Base45Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ $%*+-./:"

// ErrBase45 wraps every base45 decoding failure.
var ErrBase45 = errors.New("base45")

var b45Index [256]int16

func init() {
	for i := range b45Index {
		b45Index[i] = -1
	}
	for i := 0; i < len(Base45Alphabet); i++ {
		b45Index[Base45Alphabet[i]] = int16(i)
	}
}

// Base45Encode encodes two bytes as three characters, least significant
// first, and a trailing odd byte as two.
func Base45Encode(data []byte) string {
	var sb strings.Builder
	n := len(data)
	sb.Grow((n/2)*3 + 2)
	for i := 0; i+1 < n; i += 2 {
		v := int(data[i])<<8 | int(data[i+1])
		c := v % 45
		v /= 45
		d := v % 45
		e := v / 45
		sb.WriteByte(Base45Alphabet[c])
		sb.WriteByte(Base45Alphabet[d])
		sb.WriteByte(Base45Alphabet[e])
	}
	if n&1 == 1 {
		v := int(data[n-1])
		sb.WriteByte(Base45Alphabet[v%45])
		sb.WriteByte(Base45Alphabet[v/45])
	}
	return sb.String()
}

// Base45Decode is the strict inverse of Base45Encode: it rejects a length
// that is 1 modulo 3, any character outside the alphabet, a triplet above
// 0xFFFF and a pair above 0xFF.
func Base45Decode(s string) ([]byte, error) {
	if len(s)%3 == 1 {
		return nil, fmt.Errorf("%w: invalid length %d", ErrBase45, len(s))
	}
	vals := make([]int, len(s))
	for i := 0; i < len(s); i++ {
		v := b45Index[s[i]]
		if v < 0 {
			return nil, fmt.Errorf("%w: invalid character %q", ErrBase45, s[i])
		}
		vals[i] = int(v)
	}
	out := make([]byte, 0, len(s)/3*2+1)
	for i := 0; i+2 < len(vals); i += 3 {
		v := vals[i] + vals[i+1]*45 + vals[i+2]*2025
		if v > 0xFFFF {
			return nil, fmt.Errorf("%w: triplet out of range", ErrBase45)
		}
		out = append(out, byte(v>>8), byte(v))
	}
	if len(vals)%3 == 2 {
		v := vals[len(vals)-2] + vals[len(vals)-1]*45
		if v > 0xFF {
			return nil, fmt.Errorf("%w: pair out of range", ErrBase45)
		}
		out = append(out, byte(v))
	}
	return out, nil
}
