package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aliaankhowaja/sockt-backend/internal/store"
)

// handleStripeWebhook verifies Stripe-Signature and credits users on payment_intent.succeeded.
func handleStripeWebhook(d RouterDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		secret := strings.TrimSpace(d.Cfg.StripeWebhookSecret)
		if secret == "" {
			http.Error(w, "stripe not configured", http.StatusServiceUnavailable)
			return
		}
		payload, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "body")
			return
		}
		sig := r.Header.Get("Stripe-Signature")
		if err := verifyStripePayload(payload, sig, secret); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_signature", err.Error())
			return
		}
		var ev struct {
			Type string `json:"type"`
			Data struct {
				Object map[string]any `json:"object"`
			} `json:"data"`
		}
		if err := json.Unmarshal(payload, &ev); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		if ev.Type != "payment_intent.succeeded" {
			w.WriteHeader(http.StatusOK)
			return
		}
		meta, _ := ev.Data.Object["metadata"].(map[string]any)
		var userIDStr string
		if meta != nil {
			if v, ok := meta["user_id"].(string); ok {
				userIDStr = v
			}
		}
		if userIDStr == "" {
			w.WriteHeader(http.StatusOK)
			return
		}
		amount := int64(0)
		if v, ok := ev.Data.Object["amount_received"]; ok {
			switch t := v.(type) {
			case float64:
				amount = int64(t)
			}
		}
		if amount <= 0 {
			w.WriteHeader(http.StatusOK)
			return
		}
		uid, err := uuid.Parse(userIDStr)
		if err != nil {
			w.WriteHeader(http.StatusOK)
			return
		}
		pi, _ := ev.Data.Object["id"].(string)
		var piPtr *string
		if pi != "" {
			piPtr = &pi
		}
		desc := "stripe payment_intent.succeeded"
		if _, err := d.Store.AddUserCredits(r.Context(), uid, amount*store.SubcentsPerCent, "purchase", desc, piPtr); err != nil {
			writeErr(w, http.StatusInternalServerError, "credit_failed", err.Error())
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

func verifyStripePayload(payload []byte, sigHeader, secret string) error {
	if sigHeader == "" {
		return fmt.Errorf("missing signature")
	}
	now := time.Now().Unix()
	for _, part := range strings.Split(sigHeader, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		if kv[0] != "t" && kv[0] != "v1" {
			continue
		}
		if kv[0] == "t" {
			ts, err := strconv.ParseInt(kv[1], 10, 64)
			if err != nil {
				return err
			}
			if now-ts > 300 || ts-now > 60 {
				return fmt.Errorf("timestamp skew")
			}
		}
	}
	var t, v1 string
	for _, part := range strings.Split(sigHeader, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			t = kv[1]
		case "v1":
			v1 = kv[1]
		}
	}
	if t == "" || v1 == "" {
		return fmt.Errorf("incomplete stripe signature header")
	}
	signed := t + "." + string(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(signed))
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(v1)) {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}
