package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestL402MacaroonRoundTrip(t *testing.T) {
	root := make([]byte, 32)
	if _, err := rand.Read(root); err != nil {
		t.Fatal(err)
	}
	agent := uuid.New()
	preimage := make([]byte, 32)
	if _, err := rand.Read(preimage); err != nil {
		t.Fatal(err)
	}
	ph := sha256.Sum256(preimage)
	phHex := hex.EncodeToString(ph[:])

	exp := time.Now().Add(5 * time.Minute)
	macWire, err := buildL402Macaroon(root, agent, phHex, exp)
	if err != nil {
		t.Fatal(err)
	}
	gotID, gotPH, err := verifyL402Macaroon(root, macWire, hex.EncodeToString(preimage))
	if err != nil {
		t.Fatal(err)
	}
	if gotID != agent {
		t.Fatalf("agent id %v != %v", gotID, agent)
	}
	if gotPH != phHex {
		t.Fatalf("ph %q != %q", gotPH, phHex)
	}
}

func TestL402MacaroonWrongPreimage(t *testing.T) {
	root := make([]byte, 32)
	_, _ = rand.Read(root)
	agent := uuid.New()
	preimage := make([]byte, 32)
	_, _ = rand.Read(preimage)
	sum := sha256.Sum256(preimage)
	phHex := hex.EncodeToString(sum[:])
	macWire, err := buildL402Macaroon(root, agent, phHex, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	wrong := make([]byte, 32)
	_, _ = rand.Read(wrong)
	if _, _, err := verifyL402Macaroon(root, macWire, hex.EncodeToString(wrong)); err == nil {
		t.Fatal("expected preimage error")
	}
}

func TestJWTDataMintParsable(t *testing.T) {
	sec := []byte("01234567890123456789012345678901")
	tok, err := MintDataJWT(sec, "sb1", "pod1", "exec", time.Hour, "")
	if err != nil {
		t.Fatal(err)
	}
	if tok == "" {
		t.Fatal("empty token")
	}
}
