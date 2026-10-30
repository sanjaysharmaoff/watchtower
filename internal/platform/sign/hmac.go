// Package sign implements the approval-link signer.
package sign

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"

	"github.com/sanjaysharmaoff/watchtower/internal/ports"
)

type hmacSigner struct{ secret []byte }

// HMAC returns a ports.Signer backed by HMAC-SHA256.
func HMAC(secret string) ports.Signer { return hmacSigner{secret: []byte(secret)} }

func (s hmacSigner) Sign(payload string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s hmacSigner) Verify(payload, signature string) bool {
	expected := s.Sign(payload)
	return hmac.Equal([]byte(expected), []byte(signature))
}
