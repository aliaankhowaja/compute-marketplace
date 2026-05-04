package lnbot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientCreateInvoice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/wallets/current/invoices" || r.Method != http.MethodPost {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 1, "status": "pending", "amount": 1, "bolt11": "lnbc1fake",
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "wk_test", "", 5*time.Second)
	if c == nil {
		t.Fatal("nil client")
	}
	b11, err := c.CreateInvoice(context.Background(), 1, "m", "")
	if err != nil {
		t.Fatal(err)
	}
	if b11 != "lnbc1fake" {
		t.Fatalf("bolt11 %q", b11)
	}
}

func TestClientSendPaymentSettled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "settled", "preimage": "abc123",
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "wk_test", "", 5*time.Second)
	pre, err := c.SendPayment(context.Background(), "lnbc1x", "idem-1")
	if err != nil {
		t.Fatal(err)
	}
	if pre != "abc123" {
		t.Fatalf("preimage %q", pre)
	}
}
