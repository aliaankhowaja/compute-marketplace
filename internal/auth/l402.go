package auth

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"gopkg.in/macaroon.v2"

	"github.com/aliaankhowaja/sockt-backend/internal/payment"
)

// Bolt11Mint mints a small prepaid invoice for L402 challenges.
type Bolt11Mint interface {
	MintL402Invoice(ctx context.Context) (*payment.Bolt11Invoice, []byte, error)
}

// L402Middleware enforces L402 macaroons or returns HTTP 402 challenge JSON.
func L402Middleware(mint Bolt11Mint, rootKey []byte, next http.Handler) http.Handler {
	rk := append([]byte(nil), rootKey...)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agHeader := strings.TrimSpace(r.Header.Get("X-Agent-Id"))
		authz := strings.TrimSpace(r.Header.Get("Authorization"))

		macB64, preHex, ok := parseL402Auth(authz)
		if !ok || macB64 == "" || preHex == "" {
			agentID, err := uuid.Parse(agHeader)
			if err != nil {
				http.Error(w, "missing or invalid X-Agent-Id for L402 challenge", http.StatusBadRequest)
				return
			}
			writeL402Challenge(w, r, mint, rk, agentID)
			return
		}

		agentID, payHashHex, err := verifyL402Macaroon(rk, macB64, preHex)
		if err != nil {
			http.Error(w, "invalid L402 macaroon", http.StatusUnauthorized)
			return
		}
		_ = payHashHex

		aid := agentID
		p := &Principal{
			Kind:       KindL402,
			AgentID:    &aid,
			BalanceKey: "agent:" + aid.String(),
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

func parseL402Auth(h string) (macB64, preimageHex string, ok bool) {
	if !strings.HasPrefix(strings.ToUpper(h), "L402 ") {
		return "", "", false
	}
	rest := strings.TrimSpace(h[5:])
	parts := strings.SplitN(rest, ":", 2)
	if len(parts) != 2 {
		return "", "", true
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), true
}

func writeL402Challenge(w http.ResponseWriter, r *http.Request, mint Bolt11Mint, rootKey []byte, agentID uuid.UUID) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if mint == nil {
		http.Error(w, "payment not configured", http.StatusServiceUnavailable)
		return
	}
	inv, _, err := mint.MintL402Invoice(ctx)
	if err != nil || inv == nil {
		http.Error(w, "failed to mint invoice", http.StatusBadGateway)
		return
	}
	phHex := hex.EncodeToString(inv.RHash)
	exp := time.Now().Add(10 * time.Minute)
	macWire, err := buildL402Macaroon(rootKey, agentID, phHex, exp)
	if err != nil {
		http.Error(w, "failed to mint macaroon", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusPaymentRequired)
	_, _ = fmt.Fprintf(w, `{"macaroon":"%s","invoice":"%s","payment_hash":"%s","expires_at":"%s"}`+"\n",
		macWire, jsonEscape(inv.PaymentRequest), phHex, exp.UTC().Format(time.RFC3339))
}

func jsonEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

func buildL402Macaroon(rootKey []byte, agentID uuid.UUID, payHashHex string, exp time.Time) (string, error) {
	if len(rootKey) < 16 {
		return "", errors.New("short root key")
	}
	m, err := macaroon.New(rootKey, []byte(agentID.String()), "sockt", macaroon.LatestVersion)
	if err != nil {
		return "", err
	}
	if err := m.AddFirstPartyCaveat([]byte("service sockt")); err != nil {
		return "", err
	}
	if err := m.AddFirstPartyCaveat([]byte("payment_hash " + strings.ToLower(strings.TrimSpace(payHashHex)))); err != nil {
		return "", err
	}
	if err := m.AddFirstPartyCaveat([]byte("exp " + exp.UTC().Format(time.RFC3339))); err != nil {
		return "", err
	}
	raw, err := m.MarshalBinary()
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(raw), nil
}

func verifyL402Macaroon(rootKey []byte, macB64, preimageHex string) (agentID uuid.UUID, payHash string, err error) {
	raw, err := base64.URLEncoding.DecodeString(macB64)
	if err != nil {
		return uuid.UUID{}, "", err
	}
	var mm macaroon.Macaroon
	if err := mm.UnmarshalBinary(raw); err != nil {
		return uuid.UUID{}, "", err
	}
	m := &mm
	agentID, err = uuid.Parse(string(m.Id()))
	if err != nil {
		return uuid.UUID{}, "", err
	}
	preimage, err := hex.DecodeString(strings.TrimSpace(preimageHex))
	if err != nil || len(preimage) != 32 {
		return uuid.UUID{}, "", errors.New("preimage")
	}
	var phDecoded string
	check := func(caveat string) error {
		caveat = strings.TrimSpace(caveat)
		switch {
		case strings.HasPrefix(caveat, "payment_hash "):
			phDecoded = strings.TrimSpace(strings.TrimPrefix(caveat, "payment_hash "))
			phBytes, err := hex.DecodeString(phDecoded)
			if err != nil || len(phBytes) != 32 {
				return errors.New("bad payment_hash caveat")
			}
			if !payment.VerifyPreimage(preimage, phBytes) {
				return errors.New("preimage mismatch")
			}
		case strings.HasPrefix(caveat, "exp "):
			ts := strings.TrimSpace(strings.TrimPrefix(caveat, "exp "))
			t, err := time.Parse(time.RFC3339, ts)
			if err != nil {
				return err
			}
			if time.Now().After(t) {
				return errors.New("macaroon expired")
			}
		case caveat == "service sockt":
			return nil
		default:
			return fmt.Errorf("unknown caveat %q", caveat)
		}
		return nil
	}
	if err := m.Verify(rootKey, check, nil); err != nil {
		return uuid.UUID{}, "", err
	}
	return agentID, phDecoded, nil
}
