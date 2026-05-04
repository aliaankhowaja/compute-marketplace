package api

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aliaankhowaja/sockt-backend/internal/auth"
	"github.com/aliaankhowaja/sockt-backend/internal/lightningwallet"
	"github.com/aliaankhowaja/sockt-backend/internal/orchestrator"
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

// handleWalletOffer registers / returns the Lightning agent row for dashboard JWT holders (prep BOLT11 deposits).
func handleWalletOffer(d RouterDeps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		if p == nil || p.UserID == nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "")
			return
		}
		var raw json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&raw)
		ag, err := d.Store.GetAgentByJWTSub(r.Context(), *p.UserID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		if ag == nil {
			a, err := d.Store.CreateAgent(r.Context(), *p.UserID)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
				return
			}
			writeJSON(w, http.StatusOK, a)
			return
		}
		writeJSON(w, http.StatusOK, ag)
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
			writeErr(w, http.StatusNotFound, "not_found", "register lightning wallet first (POST /v1/wallet/offer)")
			return
		}
		rate, err := d.Store.MaxMsatsPerSecondRunningSandboxesForAgent(r.Context(), ag.ID)
		if err != nil || rate <= 0 {
			pr, err2 := d.Store.GetTierPricing(r.Context(), "nano")
			if err2 != nil || pr == nil {
				writeErr(w, http.StatusInternalServerError, "store_error", "tier pricing")
				return
			}
			rate = pr.MsatsPerSecond
		}
		nextAction, minDep, sug := lightningwallet.Hints(ag.BalanceMsats, rate, d.Cfg)
		cpOrigin := composeDepositHintURL(d.Cfg.ControlPlanePublicURL)
		if cpOrigin == "" {
			cpOrigin = composeDepositHintURL("http://127.0.0.1:" + d.Cfg.Port)
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"agent_id":                ag.ID,
			"balance_msats":           ag.BalanceMsats,
			"warning_level":           ag.WarningLevel,
			"next_action":             nextAction,
			"min_deposit_msats":       minDep,
			"suggested_deposit_msats": sug,
			"deposit_url":             cpOrigin,
			"billing_hint_msats_sec":  rate,
		})
	}
}

type walletDepositBody struct {
	AmountSats     int64  `json:"amount_sats"`
	IdempotencyKey string `json:"idempotency_key"`
}

func handleWalletDeposit(d RouterDeps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		if p == nil || p.UserID == nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "")
			return
		}
		if d.PaySvc == nil || !d.PaySvc.HasLightning() {
			writeErr(w, http.StatusServiceUnavailable, "payment_disabled", "lightning wallet not configured")
			return
		}
		ag, err := d.Store.GetAgentByJWTSub(r.Context(), *p.UserID)
		if err != nil || ag == nil {
			writeErr(w, http.StatusNotFound, "not_found", "register wallet first")
			return
		}
		var body walletDepositBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		expiry := int64(3600)
		memo := fmt.Sprintf("wallet-deposit:user:%s:agent:%s", p.UserID.String(), ag.ID.String())
		dep, err := d.PaySvc.RequestDepositInvoice(r.Context(), ag.ID, body.AmountSats, expiry, memo, strings.TrimSpace(body.IdempotencyKey))
		if err != nil {
			writeErr(w, http.StatusBadGateway, "invoice_failed", err.Error())
			return
		}
		if len(dep.RHash) != 32 {
			writeErr(w, http.StatusBadGateway, "invoice_bad_hash", "")
			return
		}
		expRFC := time.Now().UTC().Add(time.Duration(dep.ExpirySeconds) * time.Second).Format(time.RFC3339)
		writeJSON(w, http.StatusOK, map[string]any{
			"bolt11":           dep.PaymentRequest,
			"payment_hash":     hex.EncodeToString(dep.RHash),
			"amount_msats":     dep.AmountMsats,
			"expires_at":       expRFC,
			"min_amount_msats": int64(1000),
		})
	}
}

func handleWalletDepositStatus(d RouterDeps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		if p == nil || p.UserID == nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "")
			return
		}
		ag, err := d.Store.GetAgentByJWTSub(r.Context(), *p.UserID)
		if err != nil || ag == nil {
			writeErr(w, http.StatusNotFound, "not_found", "register wallet first")
			return
		}
		hhex := strings.TrimSpace(r.PathValue("payment_hash"))
		ph, err := hex.DecodeString(hhex)
		if err != nil || len(ph) != 32 {
			writeErr(w, http.StatusBadRequest, "bad_request", "payment_hash")
			return
		}
		st, err := d.Store.GetDepositStatusForAgent(r.Context(), ag.ID, ph)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		if st == nil {
			writeErr(w, http.StatusNotFound, "not_found", "unknown payment_hash")
			return
		}
		out := map[string]any{
			"status":       st.Status,
			"amount_msats": st.AmountMsats,
			"settled_at":   nil,
			"bolt11":       nil,
		}
		if st.SettledAt != nil {
			out["settled_at"] = st.SettledAt.UTC().Format(time.RFC3339)
		}
		if st.Bolt11Invoice != nil {
			out["bolt11"] = *st.Bolt11Invoice
		}
		writeJSON(w, http.StatusOK, out)
	}
}

type createSandboxBody struct {
	BillingMethod string `json:"billing_method"`
	Tier          string `json:"tier"`
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
		tier := strings.TrimSpace(body.Tier)
		if tier == "" {
			writeErr(w, http.StatusBadRequest, "bad_request", "tier required")
			return
		}
		pr, err := d.Store.GetTierPricing(r.Context(), tier)
		if err != nil || pr == nil {
			writeErr(w, http.StatusBadRequest, "bad_tier", "unknown tier")
			return
		}
		_, templateLabel, runtime, err := orchestrator.ResolveTier(d.Cfg, tier)
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, "orchestrator_disabled", err.Error())
			return
		}

		sb := &store.Sandbox{
			BillingMethod: bm,
			Tier:          tier,
			Template:      templateLabel,
			Status:        "pending",
			Runtime:       runtime,
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
			"JWT_AUDIENCE":           d.Cfg.DataJWTAudience,
			"SANDBOX_ROOT":           d.Cfg.RuntimeSandboxRoot,
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
		if err := d.Orch.CreateSandbox(r.Context(), sb, env); err != nil {
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
		tok, err := auth.MintDataJWT(d.Cfg.WSSJWTSecret, sb.ID.String(), *sb.PodID, "all", d.Cfg.DataJWTTTL, d.Cfg.DataJWTAudience)
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
			if d.PaySvc == nil || !d.PaySvc.HasLightning() || p == nil || p.AgentID == nil {
				writeErr(w, http.StatusBadRequest, "bad_request", "")
				return
			}
			sats := body.Cents
			if sats <= 0 {
				sats = 50_000
			}
			idem := fmt.Sprintf("extend:sandbox:%s:%d", id.String(), time.Now().UnixNano())
			memo := fmt.Sprintf("extend-balance:sandbox:%s", id.String())
			dep, err := d.PaySvc.RequestDepositInvoice(r.Context(), *p.AgentID, sats, 3600, memo, idem)
			if err != nil {
				writeErr(w, http.StatusBadGateway, string(payment.Classify(err)), err.Error())
				return
			}
			if len(dep.RHash) != 32 {
				writeErr(w, http.StatusBadGateway, "invoice_bad_hash", "")
				return
			}
			ag2, _ := d.Store.GetAgentByID(r.Context(), *p.AgentID)
			wLvl := ""
			if ag2 != nil {
				wLvl = ag2.WarningLevel
			}
			expRFC := time.Now().UTC().Add(time.Duration(dep.ExpirySeconds) * time.Second).Format(time.RFC3339)
			writeJSON(w, http.StatusOK, map[string]any{
				"bolt11":        dep.PaymentRequest,
				"payment_hash":  hex.EncodeToString(dep.RHash),
				"amount_msats":  dep.AmountMsats,
				"expires_at":    expRFC,
				"warning_level": wLvl,
			})
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
