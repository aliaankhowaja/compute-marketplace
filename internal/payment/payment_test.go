package payment

import (
	"crypto/sha256"
	"testing"
)

func TestVerifyPreimage(t *testing.T) {
	preimage := sha256.Sum256([]byte("sockt"))
	wrongHash := sha256.Sum256([]byte("other"))
	realHash := sha256.Sum256(preimage[:])
	if !VerifyPreimage(preimage[:], realHash[:]) {
		t.Fatal("expected match")
	}
	if VerifyPreimage(preimage[:], wrongHash[:]) {
		t.Fatal("expected mismatch")
	}
	if VerifyPreimage([]byte("short"), realHash[:]) {
		t.Fatal("expected length guard")
	}
}

func TestIdempotencyReplay(t *testing.T) {
	c := NewIdempotencyCache()
	c.SetSuccess("k1", "abc", 0)
	rep, hit := c.Get("k1")
	if !hit || rep == nil {
		t.Fatal("expected hit")
	}
	if rep.preimageHex != "abc" || rep.err != nil {
		t.Fatalf("unexpected replay: %#v", rep)
	}
}
