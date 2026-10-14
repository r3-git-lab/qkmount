package rclone

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

// obscureForTest implements rclone's Obscure (the inverse of Reveal) so the
// round-trip can be verified without hard-coding anyone's real password.
func obscureForTest(t *testing.T, plain string) string {
	t.Helper()
	block, err := aes.NewCipher(cryptKey)
	if err != nil {
		t.Fatal(err)
	}
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(iv); err != nil {
		t.Fatal(err)
	}
	out := make([]byte, aes.BlockSize+len(plain))
	copy(out, iv)
	cipher.NewCTR(block, iv).XORKeyStream(out[aes.BlockSize:], []byte(plain))
	return base64.RawURLEncoding.EncodeToString(out)
}
