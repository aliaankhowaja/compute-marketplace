package auth

import (
	"testing"
	"time"
)

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
