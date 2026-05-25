package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aliaankhowaja/sockt-backend/internal/auth"
	"github.com/aliaankhowaja/sockt-backend/internal/orchestrator"
	"github.com/aliaankhowaja/sockt-backend/internal/store"
)

func publicRuntime(internal string) string {
	switch internal {
	case "runpod_cpu":
		return "cpu"
	case "runpod_gpu":
		return "gpu"
	default:
		return internal
	}
}

func sanitizedSandbox(sb *store.Sandbox) map[string]any {
	m := map[string]any{
		"id":              sb.ID,
		"billing_method":  sb.BillingMethod,
		"tier":            sb.Tier,
		"template":        sb.Template,
		"status":          sb.Status,
		"runtime":         publicRuntime(sb.Runtime),
		"consumed_msats":  sb.ConsumedMsats,
	}
	if sb.AgentID != nil {
		m["agent_id"] = sb.AgentID
	}
	if sb.UserID != nil {
		m["user_id"] = sb.UserID
	}
	return m
}

func canAccessSandbox(p *auth.Principal, sb *store.Sandbox) bool {
	if p == nil || sb == nil {
		return false
	}
	if p.Kind == auth.KindAPIKey && p.UserID != nil && sb.UserID != nil {
		return *p.UserID == *sb.UserID
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
		writeJSON(w, http.StatusOK, map[string]int64{"credit_balance_usd_cents": u.CreditBalanceSubcents / store.SubcentsPerCent})
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

type createSandboxBody struct {
	BillingMethod       string `json:"billing_method"`
	Tier                string `json:"tier"`
	InitialCreditsCents int64  `json:"initial_credits_cents,omitempty"`
}

func handleCreateSandbox(d RouterDeps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFrom(r.Context())
		if p == nil || p.Kind != auth.KindAPIKey || p.UserID == nil || p.APIKeyID == nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "api key required")
			return
		}
		var body createSandboxBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "json")
			return
		}
		bm := strings.ToLower(strings.TrimSpace(body.BillingMethod))
		if bm == "" {
			bm = "credits"
		}
		if bm != "credits" {
			writeErr(w, http.StatusBadRequest, "bad_request", "REST /v1/sandbox/create only supports credits; for lightning use the MCP API")
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
		_, templateLabel, runtimeLabel, err := orchestrator.ResolveTier(d.Cfg, tier)
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, "orchestrator_disabled", err.Error())
			return
		}

		sb := &store.Sandbox{
			BillingMethod: "credits",
			Tier:          tier,
			Template:      templateLabel,
			Status:        "pending",
			Runtime:       runtimeLabel,
			UserID:        p.UserID,
			APIKeyID:      p.APIKeyID,
		}
		if err := d.Store.InsertSandbox(r.Context(), sb); err != nil {
			writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		minSubcents := pr.USDCentsPerSecondX100 * 3600
		if minSubcents <= 0 {
			minSubcents = store.SubcentsPerCent
		}
		allocRequest := body.InitialCreditsCents * store.SubcentsPerCent
		if allocRequest <= 0 {
			allocRequest = minSubcents
		}
		if allocRequest < minSubcents {
			writeErr(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("minimum allocation is %d cents (1 hour of compute)", minSubcents/store.SubcentsPerCent))
			return
		}
		alloc, _, _, err := d.Store.AllocateCreditsForSandbox(r.Context(), *sb.UserID, sb.ID, allocRequest)
		if err != nil {
			writeErr(w, http.StatusPaymentRequired, "insufficient_credits", err.Error())
			return
		}
		rate := pr.USDCentsPerSecondX100
		if rate <= 0 {
			rate = 1
		}
		env := orchestrator.BuildSandboxEnv(d.Cfg, sb, alloc, rate)
		if d.Orch == nil {
			writeErr(w, http.StatusServiceUnavailable, "orchestrator_disabled", "sandbox orchestration not configured")
			return
		}
		if err := d.Orch.CreateSandbox(r.Context(), sb, env); err != nil {
			_, _ = d.Store.RefundSandboxCredits(r.Context(), *sb.UserID, sb.ID)
			writeErr(w, http.StatusBadGateway, "provisioning_error", err.Error())
			return
		}
		sb2, _ := d.Store.GetSandbox(r.Context(), sb.ID)
		writeJSON(w, http.StatusOK, sanitizedSandbox(sb2))
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
		writeJSON(w, http.StatusOK, sanitizedSandbox(sb))
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
		if err := d.Orch.ResumeSandbox(r.Context(), podID, id, sb.Tier); err != nil {
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

		var body struct {
			LightningAddress string `json:"lightning_address"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
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

		addr := strings.TrimSpace(body.LightningAddress)
		var refundResult map[string]any
		if sb.BillingMethod == "lightning" && addr != "" && sb.PrepaidBalanceMsats > 0 && d.PaySvc != nil {
			refundSats := sb.PrepaidBalanceMsats / 1000
			if sb.PrepaidBalanceMsats%1000 != 0 {
				refundSats++
			}
			if refundSats > 0 {
				idem := "sandbox-refund:" + id.String()
				if err := d.PaySvc.SendToAddress(r.Context(), addr, refundSats, idem); err != nil {
					refundResult = map[string]any{"status": "failed", "reason": err.Error(), "amount_sats": refundSats}
				} else {
					refundResult = map[string]any{"status": "sent", "amount_sats": refundSats, "address": addr}
				}
			}
		}

		if sb.BillingMethod == "credits" && sb.UserID != nil {
			refundedCents, refundErr := d.Store.RefundSandboxCredits(r.Context(), *sb.UserID, sb.ID)
			if refundErr != nil {
				slog.Error("credit refund failed", "sandbox_id", id, "err", refundErr)
			}
			if refundedCents > 0 {
				refundResult = map[string]any{"status": "credited", "amount_cents": refundedCents / store.SubcentsPerCent}
			}
		}

		response := map[string]any{
			"sandbox_id":           id.String(),
			"status":               "terminated",
			"consumed_msats_total": sb.ConsumedMsats,
			"terminated_at":        time.Now().Format(time.RFC3339),
		}
		if refundResult != nil {
			response["refund"] = refundResult
		}
		writeJSON(w, http.StatusOK, response)
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
		if sb.BillingMethod != "credits" || p == nil || p.UserID == nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "credits sandboxes only")
			return
		}
		var body extendBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		want := body.Cents * store.SubcentsPerCent
		if want <= 0 {
			want = d.Cfg.LowCreditThresholdCents * store.SubcentsPerCent * 10
		}
		a, poolAfter, _, err := d.Store.AllocateCreditsForSandbox(r.Context(), *p.UserID, id, want)
		if err != nil {
			writeErr(w, http.StatusPaymentRequired, "insufficient_credits", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]int64{"allocated_cents": a / store.SubcentsPerCent, "sandbox_pool_cents": poolAfter / store.SubcentsPerCent})
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
		writeJSON(w, http.StatusOK, map[string]any{"intervals": []any{}})
	}
}
