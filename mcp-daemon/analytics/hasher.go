// Package analytics implements the Zero Knowledge Architecture (ZKA) processing
// pipeline for BurnoutRadar. No raw PII (user IDs, message text) ever leaves
// this package in plaintext — only opaque hashes and aggregate scalars are
// propagated downstream.
package analytics

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// saltSize is the number of cryptographically random bytes used as the daily
// HMAC salt. 32 bytes = 256-bit entropy, making preimage attacks infeasible.
const saltSize = 32

// Hasher wraps a single-use, ephemeral HMAC key (the "daily salt") that is
// generated fresh at runtime using crypto/rand. The salt is NEVER persisted to
// disk, logged, or transmitted — it lives exclusively in this struct's memory.
// When the process exits the salt is gone, making it impossible to re-identify
// any hashed user ID after the fact (forward secrecy at the identity layer).
type Hasher struct {
	// salt is the raw random bytes used as the HMAC key.
	// ZKA: This field must NEVER be serialised, logged, or exposed via any API.
	salt []byte
}

// NewHasher creates a Hasher with a freshly generated cryptographic salt.
// It uses crypto/rand (not math/rand) so the salt is suitable for security use.
// Returns an error if the OS entropy source fails (extremely rare in practice).
func NewHasher() (*Hasher, error) {
	// ZKA: Generate salt purely in RAM — no file write, no env var, no seed file.
	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("hasher: failed to generate cryptographic salt: %w", err)
	}

	return &Hasher{salt: salt}, nil
}

// HashUserID accepts a raw user identifier (e.g. Slack user_id "U012AB3CD")
// and returns an HMAC-SHA256 hex digest keyed by the ephemeral daily salt.
//
// ZKA guarantees provided by this function:
//  1. The raw `rawID` string is consumed as []byte locally and NOT stored.
//  2. The returned hex string is a one-way transformation — without the salt
//     (which never persists) the original ID cannot be recovered.
//  3. The same rawID maps to the same hash within a single process lifetime,
//     ensuring correct per-channel counting WITHOUT storing the real identity.
func (h *Hasher) HashUserID(rawID string) string {
	// ZKA: rawID is used only as HMAC input, never assigned to a struct field
	// or appended to any buffer that outlives this stack frame.
	mac := hmac.New(sha256.New, h.salt) // keyed with ephemeral salt
	mac.Write([]byte(rawID))            // feed raw bytes; rawID not retained
	digest := mac.Sum(nil)              // produce 32-byte HMAC output

	// Return hex encoding — a deterministic but non-reversible token.
	// This token is safe to use as a map key without revealing the user's identity.
	return hex.EncodeToString(digest)
}
