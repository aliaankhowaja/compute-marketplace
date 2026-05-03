package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/aliaankhowaja/sockt-backend/internal/payment"
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
			if sb.AgentID == nil {
				writeErr(w, http.StatusBadRequest, "bad_request", "no agent")
				return
			}
			var rows []store.BillingInterval
			var total int64
			for _, iv := range body.Intervals {
				ms := iv.Consumed
				if ms <= 0 {
					continue
				}
				total += ms
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
			if sb.AgentID == nil || d.PaySvc == nil {
				writeJSON(w, http.StatusOK, map[string]any{"balance_delta": int64(0), "error_class": "disabled"})
				return
			}
			sats := body.RequestedTopup
			if sats <= 0 {
				sats = body.Balance / 1000
				if sats <= 0 {
					sats = 50_000
				}
			}
			idem := fmt.Sprintf("topup:%s:%s:%d", sid.String(), strings.TrimSpace(body.PodID), body.ConsumedTotal)
			preHex, err := d.PaySvc.TopUpBalance(r.Context(), *sb.AgentID, sats, idem)
			if err != nil {
				writeJSON(w, http.StatusOK, map[string]any{
					"balance_delta": int64(0),
					"error_class":   string(payment.Classify(err)),
					"message":       err.Error(),
				})
				return
			}
			preBytes, err := hex.DecodeString(strings.TrimSpace(preHex))
			if err != nil || len(preBytes) != 32 {
				writeJSON(w, http.StatusOK, map[string]any{"balance_delta": int64(0), "error_class": "preimage"})
				return
			}
			sum := sha256.Sum256(preBytes)
			msats := sats * 1000
			if err := d.Store.ApplyAutonomousTopUp(r.Context(), *sb.AgentID, idem, msats, sum[:], preBytes, ""); err != nil {
				writeJSON(w, http.StatusOK, map[string]any{"balance_delta": int64(0), "error_class": "store", "message": err.Error()})
				return
			}
			ag, _ := d.Store.GetAgentByID(r.Context(), *sb.AgentID)
			bal := int64(0)
			if ag != nil {
				bal = ag.BalanceMsats
			}
			writeJSON(w, http.StatusOK, map[string]any{"balance_delta": msats, "new_balance": bal})
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
