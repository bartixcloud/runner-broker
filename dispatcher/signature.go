// Package dispatcher is the single webhook endpoint of the bartixcloud-runners
// GitHub App (spec section 3.3). It verifies the one HMAC, maps installation.id to a
// tenant, pokes nuk-converge on a v* tag push to an infra repo, and forwards every
// other tenant event to that tenant's broker over a root-only unix socket.
package dispatcher

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// signaturePrefix is the scheme GitHub puts in X-Hub-Signature-256.
const signaturePrefix = "sha256="

// VerifySignature reports whether header is GitHub's HMAC-SHA256 of body under
// secret. The comparison is constant-time; a missing header never verifies.
func VerifySignature(secret string, body []byte, header string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	// hash.Hash.Write never fails; the check keeps the gate fail-closed regardless.
	if _, err := mac.Write(body); err != nil {
		return false
	}
	expected := signaturePrefix + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(header), []byte(expected))
}
