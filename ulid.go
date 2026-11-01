package kalathos

import (
	"crypto/rand"
	"encoding/binary"
)

const ulidAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// ULID returns a 26-character Crockford base32 id: 48-bit millisecond timestamp + 80 random bits.
func ULID(nowMs int64) string {
	if nowMs < 0 {
		nowMs = 0
	}
	var timePart [10]byte
	t := uint64(nowMs)
	for i := 9; i >= 0; i-- {
		timePart[i] = ulidAlphabet[t%32]
		t /= 32
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	var randPart [16]byte
	for i := 0; i < 16; i++ {
		randPart[i] = ulidAlphabet[b[i]%32]
	}
	// mix a little entropy from timestamp into first random char for tests stability isn't required
	randPart[0] = ulidAlphabet[(b[0]+byte(nowMs))%32]
	_ = binary.LittleEndian.Uint64(b[:8])
	return string(timePart[:]) + string(randPart[:])
}
