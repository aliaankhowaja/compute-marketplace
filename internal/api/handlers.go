package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aliaankhowaja/sockt-backend/internal/auth"
	"github.com/aliaankhowaja/sockt-backend/internal/payment"
	"github.com/aliaankhowaja/sockt-backend/internal/store"
)

func canAccessSandbox(p *auth.Principal, sb *store.Sandbox) bool {
	if p == nil || sb == nil {
		return false
	}
	if p.Kind == auth.KindAPIKey && p.UserID != nil && sb.UserID != nil {
		return *p.UserID == *sb.UserID
	}
	if p.Kind == auth.KindL402 && p.AgentID != nil && sb.AgentID != nil {
		return *p.AgentID == *sb.AgentID
	}
	return false
}

func handleUserProfile(d RouterDeps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		if p == nil || p.UserID == nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "missing principal")
			return
		}
		u, err := d.Store.GetUserByID(r.Context(), *p.UserID)
		if err != nil || u == nil {
			writeErr(w, http.StatusNotFound, "not_found", "user not found")
			return
		}
		writeJSON(w, http.StatusOK, u)
	}
}

func handleUserCredits(d RouterDeps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		if p == nil || p.UserID == nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "missing principal")
			return
		}
		u, err := d.Store.GetUserByID(r.Context(), *p.UserID)
		if err != nil || u == nil {
			writeErr(w, http.StatusNotFound, "not_found", "user not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]int64{"credit_balance_usd_cents": u.CreditBalanceUSDCents})
	}
}

func handleUserCreditsHistory(d RouterDeps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		if p == nil || p.UserID == nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "missing principal")
			return
		}
		rows, err := d.Store.ListCreditTransactionsByUser(r.Context(), *p.UserID, 100)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, rows)
	}
}

type nameBody struct {
	Name string `json:"name"`
}

func handleCreateAPIKey(d RouterDeps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		if p == nil || p.UserID == nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "")
			return
		}
		var nb nameBody
		_ = json.NewDecoder(r.Body).Decode(&nb)
		if strings.TrimSpace(nb.Name) == "" {
			nb.Name = "default"
		}
		raw, k, err := d.Store.CreateAPIKey(r.Context(), *p.UserID, nb.Name)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"key": raw, "id": k.ID, "prefix": k.KeyPrefix, "name": k.Name})
	}
}

func handleListAPIKeys(d RouterDeps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		if p == nil || p.UserID == nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "")
			return
		}
		keys, err := d.Store.ListAPIKeysByUser(r.Context(), *p.UserID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		type row struct {
			ID       uuid.UUID `json:"id"`
			Prefix   string    `json:"prefix"`
			Name     string    `json:"name"`
			IsActive bool      `json:"is_active"`
		}
		out := make([]row, 0, len(keys))
		for _, k := range keys {
			out = append(out, row{ID: k.ID, Prefix: k.KeyPrefix, Name: k.Name, IsActive: k.IsActive})
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func handleRevokeAPIKey(d RouterDeps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		if p == nil || p.UserID == nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "")
			return
		}
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "invalid id")
			return
		}
		if err := d.Store.RevokeAPIKey(r.Context(), *p.UserID, id); err != nil {
			writeErr(w, http.StatusBadRequest, "revoke_failed", err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

type walletOfferBody struct {
	OfferType        string  `json:"offer_type"`
	Bolt12Offer      *string `json:"bolt12_offer"`
	LightningAddress *string `json:"lightning_address"`
}

func handleWalletOffer(d RouterDeps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		if p == nil || p.UserID == nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "")
			return
		}
		var body walletOfferBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "json")
			return
		}
		ot := strings.TrimSpace(strings.ToLower(body.OfferType))
		if ot == "" {
			ot = "bolt12"
		}
		ag, err := d.Store.GetAgentByJWTSub(r.Context(), *p.UserID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		if ag == nil {
			a, err := d.Store.CreateAgent(r.Context(), *p.UserID, ot, body.Bolt12Offer, body.LightningAddress)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
				return
			}
			writeJSON(w, http.StatusOK, a)
			return
		}
		if err := d.Store.UpdateAgentOffer(r.Context(), ag.ID, ot, body.Bolt12Offer, body.LightningAddress); err != nil {
			writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		ag2, _ := d.Store.GetAgentByID(r.Context(), ag.ID)
		writeJSON(w, http.StatusOK, ag2)
	}
}

func handleWalletBalance(d RouterDeps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		if p == nil || p.UserID == nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "")
			return
		}
		ag, err := d.Store.GetAgentByJWTSub(r.Context(), *p.UserID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		if ag == nil {
			writeErr(w, http.StatusNotFound, "not_found", "register wallet offer first")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"agent_id":          ag.ID,
			"balance_msats":     ag.BalanceMsats,
			"offer_type":        ag.OfferType,
			"lightning_address": ag.LightningAddress,
			"warning_level":     ag.WarningLevel,
		})
	}
}

type walletTopupBody struct {
	AmountSats int64 `json:"amount_sats"`
}

func handleWalletTopup(d RouterDeps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		if p == nil || p.UserID == nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "")
			return
		}
		if d.Pay == nil || d.PaySvc == nil || d.PaySvc.LND == nil {
			writeErr(w, http.StatusServiceUnavailable, "payment_disabled", "lnd not configured")
			return
		}
		ag, err := d.Store.GetAgentByJWTSub(r.Context(), *p.UserID)
		if err != nil || ag == nil {
			writeErr(w, http.StatusNotFound, "not_found", "register wallet first")
			return
		}
		var body walletTopupBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.AmountSats <= 0 {
			body.AmountSats = 50_000
		}
		inv, err := d.Pay.CreateBOLT11Invoice(r.Context(), body.AmountSats, fmt.Sprintf("deposit:%s", ag.ID), 3600)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "invoice_failed", err.Error())
			return
		}
		ph := inv.RHash
		if len(ph) != 32 {
			writeErr(w, http.StatusBadGateway, "invoice_bad_hash", "")
			return
		}
		msats := body.AmountSats * 1000
		if err := d.Store.InsertPendingDeposit(r.Context(), ag.ID, ph, inv.PaymentRequest, msats); err != nil {
			writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"bolt11":       inv.PaymentRequest,
			"payment_hash": hex.EncodeToString(ph),
			"amount_msats": msats,
		})
	}
}

type createSandboxBody struct {
	BillingMethod string `json:"billing_method"`
	Tier          string `json:"tier"`
	Template      string `json:"template"`
}

func handleCreateSandbox(d RouterDeps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		if p == nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "")
			return
		}
		var body createSandboxBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "json")
			return
		}
		bm := strings.ToLower(strings.TrimSpace(body.BillingMethod))
		if bm != "lightning" && bm != "credits" {
			writeErr(w, http.StatusBadRequest, "bad_request", "billing_method")
			return
		}
		tplName := strings.TrimSpace(body.Template)
		if tplName == "" {
			writeErr(w, http.StatusBadRequest, "bad_request", "template required")
			return
		}
		tpl, err := d.Store.GetTemplateByName(r.Context(), tplName)
		if err != nil || tpl == nil {
			writeErr(w, http.StatusBadRequest, "bad_template", "unknown template")
			return
		}
		tier := strings.TrimSpace(body.Tier)
		if tier == "" {
			tier = tpl.DefaultTier
		}
		pr, err := d.Store.GetTierPricing(r.Context(), tier)
		if err != nil || pr == nil {
			writeErr(w, http.StatusBadRequest, "bad_tier", "unknown tier")
			return
		}

		sb := &store.Sandbox{
			BillingMethod: bm,
			Tier:          tier,
			Template:      tplName,
			Status:        "pending",
			Runtime:       tpl.Runtime,
			ConsumedMsats: 0,
		}

		switch bm {
		case "credits":
			if p.Kind != auth.KindAPIKey || p.UserID == nil || p.APIKeyID == nil {
				writeErr(w, http.StatusUnauthorized, "unauthorized", "api key required for credits sandboxes")
				return
			}
			sb.UserID = p.UserID
			sb.APIKeyID = p.APIKeyID
		case "lightning":
			if p.Kind != auth.KindL402 || p.AgentID == nil {
				writeErr(w, http.StatusUnauthorized, "unauthorized", "L402 required for lightning sandboxes")
				return
			}
			sb.AgentID = p.AgentID
		}

		if err := d.Store.InsertSandbox(r.Context(), sb); err != nil {
			writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}

		cp := strings.TrimRight(strings.TrimSpace(d.Cfg.ControlPlanePublicURL), "/")
		if cp == "" {
			cp = "http://127.0.0.1:" + d.Cfg.Port
		}

		var initialBal int64
		var rate int64
		var topThresh, topAmt int64
		switch bm {
		case "lightning":
			ag, _ := d.Store.GetAgentByID(r.Context(), *sb.AgentID)
			if ag != nil {
				initialBal = ag.BalanceMsats
			}
			rate = pr.MsatsPerSecond
			topThresh = int64(d.Cfg.BalanceLowWaterSecs) * rate
			topAmt = int64(d.Cfg.BalanceTopupChunkSecs) * rate
		case "credits":
			alloc, _, err := d.Store.AllocateCreditsForSandbox(r.Context(), *sb.UserID, sb.ID, d.Cfg.LowCreditThresholdCents*10)
			if err != nil {
				writeErr(w, http.StatusPaymentRequired, "insufficient_credits", err.Error())
				return
			}
			if pr.USDCentsPerSecondX100 <= 0 {
				rate = 1
			} else {
				rate = pr.USDCentsPerSecondX100 / 100
				if rate <= 0 {
					rate = 1
				}
			}
			initialBal = alloc
			topThresh = int64(d.Cfg.BalanceLowWaterSecs) * rate
			topAmt = int64(d.Cfg.BalanceTopupChunkSecs) * rate
		}

		env := map[string]string{
			"SANDBOX_ID":             sb.ID.String(),
			"POD_ID":                 "", // filled after create
			"CONTROL_PLANE_URL":      cp,
			"JWT_SECRET":             string(d.Cfg.WSSJWTSecret),
			"CALLBACK_SECRET":        d.Cfg.CallbackSecret,
			"BILLING_METHOD":         bm,
			"RATE_PER_SECOND":        fmt.Sprintf("%d", rate),
			"INITIAL_BALANCE":        fmt.Sprintf("%d", initialBal),
			"TOPUP_THRESHOLD":        fmt.Sprintf("%d", topThresh),
			"TOPUP_AMOUNT":           fmt.Sprintf("%d", topAmt),
			"BALANCE_LOW_WATER_SECS": fmt.Sprintf("%d", d.Cfg.BalanceLowWaterSecs),
		}

		if d.Orch == nil {
			writeErr(w, http.StatusServiceUnavailable, "orchestrator_disabled", "runpod not configured")
			return
		}
		if err := d.Orch.CreateSandbox(r.Context(), sb, tpl, env); err != nil {
			writeErr(w, http.StatusBadGateway, "runpod_error", err.Error())
			return
		}
		sb2, _ := d.Store.GetSandbox(r.Context(), sb.ID)
		writeJSON(w, http.StatusOK, sb2)
	}
}

func handleGetSandbox(d RouterDeps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "id")
			return
		}
		sb, err := d.Store.GetSandbox(r.Context(), id)
		if err != nil || sb == nil {
			writeErr(w, http.StatusNotFound, "not_found", "")
			return
		}
		if !canAccessSandbox(p, sb) {
			writeErr(w, http.StatusForbidden, "forbidden", "")
			return
		}
		writeJSON(w, http.StatusOK, sb)
	}
}

func handlePauseSandbox(d RouterDeps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "id")
			return
		}
		sb, err := d.Store.GetSandbox(r.Context(), id)
		if err != nil || sb == nil || !canAccessSandbox(p, sb) {
			writeErr(w, http.StatusForbidden, "forbidden", "")
			return
		}
		podID := ""
		if sb.PodID != nil {
			podID = *sb.PodID
		}
		if err := d.Orch.PauseSandbox(r.Context(), podID, id); err != nil {
			writeErr(w, http.StatusBadGateway, "pause_failed", err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleResumeSandbox(d RouterDeps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "id")
			return
		}
		sb, err := d.Store.GetSandbox(r.Context(), id)
		if err != nil || sb == nil || !canAccessSandbox(p, sb) {
			writeErr(w, http.StatusForbidden, "forbidden", "")
			return
		}
		podID := ""
		if sb.PodID != nil {
			podID = *sb.PodID
		}
		if err := d.Orch.ResumeSandbox(r.Context(), podID, id); err != nil {
			writeErr(w, http.StatusBadGateway, "resume_failed", err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleDeleteSandbox(d RouterDeps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "id")
			return
		}
		sb, err := d.Store.GetSandbox(r.Context(), id)
		if err != nil || sb == nil || !canAccessSandbox(p, sb) {
			writeErr(w, http.StatusForbidden, "forbidden", "")
			return
		}
		podID := ""
		if sb.PodID != nil {
			podID = *sb.PodID
		}
		if err := d.Orch.TerminateSandbox(r.Context(), podID, id); err != nil {
			writeErr(w, http.StatusBadGateway, "terminate_failed", err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleRefreshDataJWT(d RouterDeps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "id")
			return
		}
		sb, err := d.Store.GetSandbox(r.Context(), id)
		if err != nil || sb == nil || !canAccessSandbox(p, sb) {
			writeErr(w, http.StatusForbidden, "forbidden", "")
			return
		}
		if sb.PodID == nil || *sb.PodID == "" {
			writeErr(w, http.StatusBadRequest, "pod_missing", "no pod yet")
			return
		}
		tok, err := auth.MintDataJWT(d.Cfg.WSSJWTSecret, sb.ID.String(), *sb.PodID, "all", d.Cfg.DataJWTTTL)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "token_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data_jwt": tok, "expires_in_secs": int(d.Cfg.DataJWTTTL.Seconds())})
	}
}

type extendBody struct {
	Cents int64 `json:"cents"`
}

func handleExtendBalance(d RouterDeps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "id")
			return
		}
		sb, err := d.Store.GetSandbox(r.Context(), id)
		if err != nil || sb == nil || !canAccessSandbox(p, sb) {
			writeErr(w, http.StatusForbidden, "forbidden", "")
			return
		}
		var body extendBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch sb.BillingMethod {
		case "credits":
			if p == nil || p.UserID == nil {
				writeErr(w, http.StatusUnauthorized, "unauthorized", "")
				return
			}
			want := body.Cents
			if want <= 0 {
				want = d.Cfg.LowCreditThresholdCents * 10
			}
			a, after, err := d.Store.AllocateCreditsForSandbox(r.Context(), *p.UserID, id, want)
			if err != nil {
				writeErr(w, http.StatusPaymentRequired, "insufficient_credits", err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]int64{"allocated_cents": a, "balance_after_cents": after})
		case "lightning":
			if d.PaySvc == nil || p == nil || p.AgentID == nil {
				writeErr(w, http.StatusBadRequest, "bad_request", "")
				return
			}
			sats := body.Cents
			if sats <= 0 {
				sats = 50_000
			}
			idem := fmt.Sprintf("extend:%s:%d", id.String(), time.Now().Unix())
			pre, err := d.PaySvc.TopUpBalance(r.Context(), *p.AgentID, sats, idem)
			if err != nil {
				writeErr(w, http.StatusPaymentRequired, string(payment.Classify(err)), err.Error())
				return
			}
			preb, _ := hex.DecodeString(pre)
			ph := sha256.Sum256(preb)
			_ = d.Store.ApplyAutonomousTopUp(r.Context(), *p.AgentID, idem, sats*1000, ph[:], preb, "")
			ag, _ := d.Store.GetAgentByID(r.Context(), *p.AgentID)
			bal := int64(0)
			if ag != nil {
				bal = ag.BalanceMsats
			}
			writeJSON(w, http.StatusOK, map[string]any{"preimage": pre, "balance_msats": bal})
		default:
			writeErr(w, http.StatusBadRequest, "bad_request", "billing method")
		}
	}
}

func handleSandboxBilling(d RouterDeps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "id")
			return
		}
		sb, err := d.Store.GetSandbox(r.Context(), id)
		if err != nil || sb == nil || !canAccessSandbox(p, sb) {
			writeErr(w, http.StatusForbidden, "forbidden", "")
			return
		}
		if sb.AgentID == nil {
			writeJSON(w, http.StatusOK, map[string]any{"intervals": []any{}})
			return
		}
		rows, err := d.Store.ListBillingIntervalsByAgent(r.Context(), *sb.AgentID, 100)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"intervals": rows})
	}
}
