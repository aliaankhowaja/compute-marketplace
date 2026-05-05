package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aliaankhowaja/sockt-backend/internal/runtime"
	"github.com/aliaankhowaja/sockt-backend/internal/store"
)

type auditBatchPayload struct {
	SandboxID     string                  `json:"sandbox_id"`
	PodID         string                  `json:"pod_id"`
	BillingMethod string                  `json:"billing_method"`
	Intervals     []runtime.AuditInterval `json:"intervals"`
}

func handleAuditBatch(d RouterDeps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		var body auditBatchPayload
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "json")
			return
		}
		sid, err := uuid.Parse(strings.TrimSpace(body.SandboxID))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "sandbox_id")
			return
		}
		sb, err := d.Store.GetSandbox(r.Context(), sid)
		if err != nil || sb == nil {
			writeErr(w, http.StatusNotFound, "not_found", "sandbox")
			return
		}
		if sb.PodID == nil || !strings.EqualFold(strings.TrimSpace(*sb.PodID), strings.TrimSpace(body.PodID)) {
			writeErr(w, http.StatusForbidden, "pod_replaced", "pod id mismatch")
			return
		}

		switch strings.ToLower(strings.TrimSpace(body.BillingMethod)) {
		case "lightning":
			var total int64
			for _, iv := range body.Intervals {
				if iv.Consumed > 0 {
					total += iv.Consumed
				}
			}
			if sb.SandboxToken != nil {
				if total > 0 {
					if _, err := d.Store.IncrementSandboxPrepaidMsats(r.Context(), sid, -total); err != nil {
						writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
						return
					}
				}
				newConsumed := sb.ConsumedMsats + total
				_ = d.Store.SetSandboxConsumedMsats(r.Context(), sid, newConsumed)
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if sb.AgentID == nil {
				writeErr(w, http.StatusBadRequest, "bad_request", "lightning sandbox missing token and agent")
				return
			}
			var rows []store.BillingInterval
			for _, iv := range body.Intervals {
				ms := iv.Consumed
				if ms <= 0 {
					continue
				}
				rows = append(rows, store.BillingInterval{
					SandboxID: sid,
					AgentID:   *sb.AgentID,
					Start:     iv.StartedAt,
					End:       iv.EndedAt,
					Msats:     ms,
				})
			}
			if err := d.Store.InsertBillingIntervals(r.Context(), *sb.AgentID, rows); err != nil {
				writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
				return
			}
			maxr, _ := d.Store.MaxMsatsPerSecondRunningSandboxesForAgent(r.Context(), *sb.AgentID)
			if maxr <= 0 {
				if pr, err2 := d.Store.GetTierPricing(r.Context(), "nano"); err2 == nil && pr != nil {
					maxr = pr.MsatsPerSecond
				}
			}
			lwMs := int64(d.Cfg.BalanceLowWaterSecs) * maxr
			if lwMs > 0 {
				_ = d.Store.RefreshAgentLightningWarning(r.Context(), *sb.AgentID, lwMs)
			}
			newConsumed := sb.ConsumedMsats + total
			_ = d.Store.SetSandboxConsumedMsats(r.Context(), sid, newConsumed)
		case "credits":
			if sb.UserID == nil {
				writeErr(w, http.StatusBadRequest, "bad_request", "no user")
				return
			}
			pr, err := d.Store.GetTierPricing(r.Context(), sb.Tier)
			if err != nil || pr == nil {
				writeErr(w, http.StatusInternalServerError, "tier_error", "tier")
				return
			}
			var charges []store.CreditCharge
			for _, iv := range body.Intervals {
				if iv.Consumed <= 0 {
					continue
				}
				cents := (iv.Consumed * pr.USDCentsPerSecondX100) / (100 * pr.MsatsPerSecond)
				if cents <= 0 {
					cents = 1
				}
				charges = append(charges, store.CreditCharge{
					UserID:      *sb.UserID,
					SandboxID:   sid,
					AmountCents: cents,
					Start:       iv.StartedAt,
					End:         iv.EndedAt,
					Tier:        sb.Tier,
					Description: "audit interval",
				})
			}
			if err := d.Store.RecordCreditComputeCharges(r.Context(), charges); err != nil {
				writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
				return
			}
		default:
			writeErr(w, http.StatusBadRequest, "bad_request", "billing_method")
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

type triggerPayload struct {
	SandboxID      string `json:"sandbox_id"`
	PodID          string `json:"pod_id"`
	BillingMethod  string `json:"billing_method"`
	Balance        int64  `json:"balance"`
	ConsumedTotal  int64  `json:"consumed_total"`
	Reason         string `json:"reason,omitempty"`
	RequestedTopup int64  `json:"requested_topup,omitempty"`
}

func handleTopupTrigger(d RouterDeps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		var body triggerPayload
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"balance_delta": int64(0), "error_class": "bad_json"})
			return
		}
		sid, err := uuid.Parse(strings.TrimSpace(body.SandboxID))
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"balance_delta": int64(0), "error_class": "bad_sandbox"})
			return
		}
		sb, err := d.Store.GetSandbox(r.Context(), sid)
		if err != nil || sb == nil {
			writeJSON(w, http.StatusOK, map[string]any{"balance_delta": int64(0), "error_class": "not_found"})
			return
		}
		if sb.PodID == nil || !strings.EqualFold(strings.TrimSpace(*sb.PodID), strings.TrimSpace(body.PodID)) {
			writeJSON(w, http.StatusOK, map[string]any{"balance_delta": int64(0), "error_class": "pod_replaced"})
			return
		}

		bm := strings.ToLower(strings.TrimSpace(body.BillingMethod))
		switch bm {
		case "lightning":
			if d.PaySvc == nil || !d.PaySvc.HasLightning() {
				writeJSON(w, http.StatusOK, map[string]any{"balance_delta": int64(0), "error_class": "disabled"})
				return
			}
			if sb.SandboxToken != nil {
				handleSandboxLightningTopup(w, r, d, sb, &body)
				return
			}
			if sb.AgentID == nil {
				writeJSON(w, http.StatusOK, map[string]any{"balance_delta": int64(0), "error_class": "disabled"})
				return
			}
			chunkMsats := body.RequestedTopup
			if chunkMsats <= 0 {
				if body.Balance > 0 {
					chunkMsats = body.Balance
				}
			}
			sats := chunkMsats / 1000
			if chunkMsats%1000 != 0 {
				sats++
			}
			if sats <= 0 {
				sats = 50_000
			}
			idem := fmt.Sprintf("runtime-mint:%s:%s:%d", sid.String(), strings.TrimSpace(body.PodID), body.ConsumedTotal)
			memo := fmt.Sprintf("runtime-deposit:sandbox:%s:pod:%s", sid.String(), strings.TrimSpace(body.PodID))
			dep, err := d.PaySvc.RequestDepositInvoice(r.Context(), *sb.AgentID, sats, 3600, memo, idem)
			if err != nil || dep == nil {
				msg := ""
				if err != nil {
					msg = err.Error()
				}
				writeJSON(w, http.StatusOK, map[string]any{
					"balance_delta": int64(0),
					"error_class":   "deposit_mint_failed",
					"message":       msg,
				})
				return
			}
			expRFC := time.Now().UTC().Add(time.Duration(dep.ExpirySeconds) * time.Second).Format(time.RFC3339)
			ag, _ := d.Store.GetAgentByID(r.Context(), *sb.AgentID)
			wlvl := ""
			if ag != nil {
				wlvl = ag.WarningLevel
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"balance_delta": int64(0),
				"error_class":   "deposit_required",
				"warning_level": wlvl,
				"deposit": map[string]any{
					"bolt11":       dep.PaymentRequest,
					"payment_hash": fmt.Sprintf("%x", dep.RHash),
					"amount_msats": dep.AmountMsats,
					"expires_at":   expRFC,
				},
			})
		case "credits":
			if sb.UserID == nil {
				writeJSON(w, http.StatusOK, map[string]any{"balance_delta": int64(0), "error_class": "no_user"})
				return
			}
			want := body.RequestedTopup
			if want <= 0 {
				want = d.Cfg.LowCreditThresholdCents * 10
			}
			a, after, err := d.Store.AllocateCreditsForSandbox(r.Context(), *sb.UserID, sid, want)
			if err != nil {
				writeJSON(w, http.StatusOK, map[string]any{"balance_delta": int64(0), "error_class": "insufficient_credits", "message": err.Error()})
				return
			}
			_ = after
			writeJSON(w, http.StatusOK, map[string]any{"balance_delta": a, "new_balance": after})
		default:
			writeJSON(w, http.StatusOK, map[string]any{"balance_delta": int64(0), "error_class": "billing_method"})
		}
	}
}

// handleSandboxLightningTopup implements the new sandbox-scoped Lightning topup flow:
//  1. If the persisted prepaid balance is ahead of the runtime's snapshot, return a balance delta so
//     the meter catches up to the post-settlement number.
//  2. Otherwise, if a pending invoice exists and is still in the freshness window, return that one.
//  3. Otherwise, mint a new BOLT11, persist it on the sandbox row, and return it.
func handleSandboxLightningTopup(w http.ResponseWriter, r *http.Request, d RouterDeps, sb *store.Sandbox, body *triggerPayload) {
	now := time.Now().UTC()

	if sb.PrepaidBalanceMsats > body.Balance {
		writeJSON(w, http.StatusOK, map[string]any{
			"balance_delta": sb.PrepaidBalanceMsats - body.Balance,
			"new_balance":   sb.PrepaidBalanceMsats,
		})
		return
	}

	if sb.PendingInvoiceBolt11 != nil && sb.PendingInvoiceExpiresAt != nil && now.Before(*sb.PendingInvoiceExpiresAt) {
		amt := int64(0)
		if sb.PendingInvoiceAmountMsats != nil {
			amt = *sb.PendingInvoiceAmountMsats
		}
		ph := ""
		if sb.PendingInvoicePaymentHash != nil {
			ph = fmt.Sprintf("%x", sb.PendingInvoicePaymentHash)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"balance_delta": int64(0),
			"error_class":   "deposit_required",
			"deposit": map[string]any{
				"bolt11":       *sb.PendingInvoiceBolt11,
				"payment_hash": ph,
				"amount_msats": amt,
				"expires_at":   sb.PendingInvoiceExpiresAt.Format(time.RFC3339),
			},
		})
		return
	}

	chunkMsats := body.RequestedTopup
	if chunkMsats <= 0 {
		prepay := int64(d.Cfg.SandboxPrepaySecs)
		if prepay <= 0 {
			prepay = 30
		}
		pr, _ := d.Store.GetTierPricing(r.Context(), sb.Tier)
		if pr != nil && pr.MsatsPerSecond > 0 {
			chunkMsats = prepay * pr.MsatsPerSecond
		}
	}
	sats := chunkMsats / 1000
	if chunkMsats%1000 != 0 {
		sats++
	}
	if sats <= 0 {
		sats = 30
	}

	expirySecs := int64(3600)
	idem := fmt.Sprintf("sandbox-mint:%s:%s:%d", sb.ID.String(), strings.TrimSpace(body.PodID), body.ConsumedTotal/1_000_000_000)
	memo := fmt.Sprintf("sandbox-prepay:%s", sb.ID.String())
	dep, err := d.PaySvc.RequestSandboxDepositInvoice(r.Context(), sb.ID, sats, expirySecs, memo, idem)
	if err != nil || dep == nil {
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"balance_delta": int64(0),
			"error_class":   "deposit_mint_failed",
			"message":       msg,
		})
		return
	}
	expRFC := now.Add(time.Duration(dep.ExpirySeconds) * time.Second)
	if err := d.Store.SetSandboxPendingInvoice(r.Context(), sb.ID, dep.PaymentRequest, dep.RHash, dep.AmountMsats, expRFC); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"balance_delta": int64(0),
			"error_class":   "store_error",
			"message":       err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"balance_delta": int64(0),
		"error_class":   "deposit_required",
		"deposit": map[string]any{
			"bolt11":       dep.PaymentRequest,
			"payment_hash": fmt.Sprintf("%x", dep.RHash),
			"amount_msats": dep.AmountMsats,
			"expires_at":   expRFC.Format(time.RFC3339),
		},
	})
}

func handlePauseTrigger(d RouterDeps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		var body triggerPayload
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "json")
			return
		}
		sid, err := uuid.Parse(strings.TrimSpace(body.SandboxID))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "sandbox_id")
			return
		}
		sb, err := d.Store.GetSandbox(r.Context(), sid)
		if err != nil || sb == nil {
			writeErr(w, http.StatusNotFound, "not_found", "")
			return
		}
		if sb.PodID == nil || !strings.EqualFold(strings.TrimSpace(*sb.PodID), strings.TrimSpace(body.PodID)) {
			writeErr(w, http.StatusForbidden, "pod_replaced", "")
			return
		}
		podID := *sb.PodID
		if err := d.Orch.PauseSandbox(r.Context(), podID, sid); err != nil {
			writeErr(w, http.StatusBadGateway, "pause_failed", err.Error())
			return
		}
		am := "callback"
		_ = d.Store.InsertAuditLog(r.Context(), "runtime_pause_trigger", sb.UserID, sb.AgentID, &sid, nil, &am, map[string]any{"reason": body.Reason})
		w.WriteHeader(http.StatusNoContent)
	}
}
