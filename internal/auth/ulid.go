package auth

import (
	"crypto/rand"
	"time"
)

// crockford is the Crockford base32 alphabet used by ULID (ulidx package).
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// encodeBits writes the low `bits` bits of v as Crockford base32, most
// significant first. len(dst)*5 must be >= bits; the first char carries any
// remainder (e.g. 48 bits -> 10 chars, first char holds 3 bits).
func encodeBits(dst []byte, v uint64, bits int) {
	shift := bits
	for i := range dst {
		take := 5
		if i == 0 {
			take = bits - (len(dst)-1)*5
		}
		shift -= take
		dst[i] = crockford[(v>>shift)&((uint64(1)<<take)-1)]
	}
}

// NewULID generates a ULID identical to the `ulidx` npm package used by the
// TS server: 48-bit big-endian millisecond timestamp followed by 80 bits of
// cryptographic randomness, Crockford base32 encoded, 26 chars, uppercase.
func NewULID() string {
	var rnd [10]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		panic("auth: crypto/rand failed: " + err.Error())
	}
	ms := uint64(time.Now().UnixMilli()) & 0xFFFFFFFFFFFF // low 48 bits

	var out [26]byte
	encodeBits(out[0:10], ms, 48)
	hi := uint64(rnd[0])<<32 | uint64(rnd[1])<<24 | uint64(rnd[2])<<16 | uint64(rnd[3])<<8 | uint64(rnd[4])
	lo := uint64(rnd[5])<<32 | uint64(rnd[6])<<24 | uint64(rnd[7])<<16 | uint64(rnd[8])<<8 | uint64(rnd[9])
	encodeBits(out[10:18], hi, 40)
	encodeBits(out[18:26], lo, 40)
	return string(out[:])
}
