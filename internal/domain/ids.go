package domain

import (
	"crypto/rand"
	"fmt"
	"strings"
	"time"
)

// ID prefixes (spec section 5). An ID is "<prefix>_<ULID>", for example exe_01J9....
const (
	PrefixExecution  = "exe"
	PrefixWorkflow   = "wf"
	PrefixAlert      = "alt"
	PrefixCase       = "cas"
	PrefixObservable = "obs"
	PrefixSource     = "sr"
	PrefixEvent      = "evt"
	PrefixTask       = "tsk"
	PrefixStepRun    = "stp"
)

// SchemaVersion is the document schema version written into every document.
const SchemaVersion = 1

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewULID returns a 26-character ULID (48-bit ms timestamp + 80 random bits).
func NewULID(t time.Time) string {
	var b [16]byte
	ms := uint64(t.UnixMilli())
	b[0], b[1], b[2] = byte(ms>>40), byte(ms>>32), byte(ms>>24)
	b[3], b[4], b[5] = byte(ms>>16), byte(ms>>8), byte(ms)
	if _, err := rand.Read(b[6:]); err != nil {
		panic(fmt.Sprintf("domain: crypto/rand failed: %v", err))
	}
	// 128 bits -> 26 base32 characters (first character carries only 3 bits).
	var out [26]byte
	var hi, lo uint64
	for i := 0; i < 8; i++ {
		hi = hi<<8 | uint64(b[i])
		lo = lo<<8 | uint64(b[8+i])
	}
	for i := 25; i >= 0; i-- {
		out[i] = crockford[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}

// NewID returns a prefixed ULID, for example NewID(PrefixCase) -> "cas_01J9...".
func NewID(prefix string) string { return prefix + "_" + NewULID(time.Now().UTC()) }

// HasPrefix reports whether id looks like "<prefix>_<26 chars>".
func HasPrefix(id, prefix string) bool {
	return strings.HasPrefix(id, prefix+"_") && len(id) == len(prefix)+1+26
}
