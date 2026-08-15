package wire

import (
	"crypto/rand"
	"time"
)

// crockford is the base32 alphabet ULIDs use (no I, L, O, U).
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewMsgID returns a 26-character ULID: 48-bit millisecond timestamp plus
// 80 bits of crypto randomness, Crockford base32. Mirrored into the
// Nats-Msg-Id header on JetStream publishes for server-side dedup.
func NewMsgID() string {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	if _, err := rand.Read(b[6:]); err != nil {
		// crypto/rand never fails on supported platforms; panicking beats
		// silently issuing colliding ids.
		panic("wire: crypto/rand failed: " + err.Error())
	}

	var out [26]byte
	out[0] = crockford[(b[0]&224)>>5]
	out[1] = crockford[b[0]&31]
	out[2] = crockford[(b[1]&248)>>3]
	out[3] = crockford[(b[1]&7)<<2|(b[2]&192)>>6]
	out[4] = crockford[(b[2]&62)>>1]
	out[5] = crockford[(b[2]&1)<<4|(b[3]&240)>>4]
	out[6] = crockford[(b[3]&15)<<1|(b[4]&128)>>7]
	out[7] = crockford[(b[4]&124)>>2]
	out[8] = crockford[(b[4]&3)<<3|(b[5]&224)>>5]
	out[9] = crockford[b[5]&31]
	out[10] = crockford[(b[6]&248)>>3]
	out[11] = crockford[(b[6]&7)<<2|(b[7]&192)>>6]
	out[12] = crockford[(b[7]&62)>>1]
	out[13] = crockford[(b[7]&1)<<4|(b[8]&240)>>4]
	out[14] = crockford[(b[8]&15)<<1|(b[9]&128)>>7]
	out[15] = crockford[(b[9]&124)>>2]
	out[16] = crockford[(b[9]&3)<<3|(b[10]&224)>>5]
	out[17] = crockford[b[10]&31]
	out[18] = crockford[(b[11]&248)>>3]
	out[19] = crockford[(b[11]&7)<<2|(b[12]&192)>>6]
	out[20] = crockford[(b[12]&62)>>1]
	out[21] = crockford[(b[12]&1)<<4|(b[13]&240)>>4]
	out[22] = crockford[(b[13]&15)<<1|(b[14]&128)>>7]
	out[23] = crockford[(b[14]&124)>>2]
	out[24] = crockford[(b[14]&3)<<3|(b[15]&224)>>5]
	out[25] = crockford[b[15]&31]
	return string(out[:])
}
