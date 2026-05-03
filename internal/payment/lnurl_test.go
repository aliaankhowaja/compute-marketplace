package payment

import "testing"

func TestResolveFirstURL_LightningAddress(t *testing.T) {
	u, err := resolveFirstURL("satoshi@getalby.com")
	if err != nil {
		t.Fatal(err)
	}
	want := "https://getalby.com/.well-known/lnurlp/satoshi"
	if u != want {
		t.Fatalf("got %q want %q", u, want)
	}
}

func TestResolveFirstURL_HTTPS(t *testing.T) {
	u, err := resolveFirstURL("https://example.com/lnurlp")
	if err != nil {
		t.Fatal(err)
	}
	if u != "https://example.com/lnurlp" {
		t.Fatal(u)
	}
}
