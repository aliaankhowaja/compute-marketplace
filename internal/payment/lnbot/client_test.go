package lnbot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestClientGetInvoiceSettledWithoutPreimage(t *testing.T) {
	const ph = "abababababababababababababababababababababababababababababababab"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "/v1/wallets/current/invoices/" + ph
		if r.URL.Path != want || r.Method != http.MethodGet {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":       "settled",
			"amount":       1,
			"bolt11":       "lnbc10n1fake",
			"memo":         "m",
			"preimage":     nil,
			"paymentHash":  ph,
			"settledAt":    "2026-01-15T11:02:00Z",
			"payment_hash": nil,
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "wk_test", "", 5*time.Second)
	inv, err := c.GetInvoiceByPaymentHash(context.Background(), ph)
	if err != nil {
		t.Fatal(err)
	}
	if inv == nil {
		t.Fatal("nil invoice")
	}
	if inv.Status != "settled" {
		t.Fatalf("status %q", inv.Status)
	}
	if strings.TrimSpace(inv.PreimageHex) != "" {
		t.Fatalf("expected empty preimage, got %q", inv.PreimageHex)
	}
}
