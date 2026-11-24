package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// Historical implementation, retained only as an independent compatibility
// and performance baseline. Both variants use exactly the same work factor.
func legacyPasswordKey(pass string, salt []byte, count int) []byte {
	key := []byte(pass)
	var ctr [4]byte
	ctr[3] = 1
	h := hmac.New(sha256.New, key)
	h.Write(salt)
	h.Write(ctr[:])
	u := h.Sum(nil)
	out := append([]byte{}, u...)
	for i := 1; i < count; i++ {
		h = hmac.New(sha256.New, key)
		h.Write(u)
		u = h.Sum(u[:0])
		for j := range out {
			out[j] ^= u[j]
		}
	}
	return out
}

func TestPasswordDerivationCompatibility(t *testing.T) {
	for _, tc := range []struct {
		iterations int
		want       string
	}{
		{1, "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"},
		{2, "ae4d0c95af6b46d32d0adff928f06dd02a303f8ef3c251dfd6e2d85a95474c43"},
		{4096, "c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134a"},
	} {
		if got := hex.EncodeToString(passwordKey("password", []byte("salt"), tc.iterations)); got != tc.want {
			t.Fatalf("PBKDF2 vector %d: %s", tc.iterations, got)
		}
	}
	salt := bytes.Repeat([]byte{42}, 24)
	if !bytes.Equal(passwordKey("legacy-password", salt, passwordIterations),
		legacyPasswordKey("legacy-password", salt, passwordIterations)) {
		t.Fatal("existing password hashes are incompatible")
	}
}

func BenchmarkPasswordDerivation(b *testing.B) {
	salt := bytes.Repeat([]byte{42}, 24)
	for _, impl := range []struct {
		name string
		key  func(string, []byte, int) []byte
	}{
		{"legacy", legacyPasswordKey},
		{"reused-HMAC", passwordKey},
	} {
		b.Run(impl.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = impl.key("benchmark-password", salt, passwordIterations)
			}
		})
	}
}
