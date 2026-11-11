package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// ObservableID is deterministic, so the same indicator always maps to the same document.
func ObservableID(typ, value string) string {
	sum := sha256.Sum256([]byte(typ + "\x00" + strings.ToLower(strings.TrimSpace(value))))
	return PrefixObservable + "_" + hex.EncodeToString(sum[:16])
}
