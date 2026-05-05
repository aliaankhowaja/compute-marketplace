package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/aliaankhowaja/sockt-backend/internal/config"
	"github.com/aliaankhowaja/sockt-backend/internal/store"
)

// BuildSandboxEnv assembles the env map injected into a RunPod sandbox pod for the meter and runtime.
// pricing must already be resolved for the sandbox tier; initialBalance is in msats for lightning or
// in cents for credits — the meter just consumes RatePerSecond from this number every tick.
func BuildSandboxEnv(cfg *config.Control, sb *store.Sandbox, initialBalance, ratePerSecond int64) map[string]string {
	cp := strings.TrimRight(strings.TrimSpace(cfg.ControlPlanePublicURL), "/")
	if cp == "" {
		cp = "http://127.0.0.1:" + cfg.Port
	}

	prepaySecs := cfg.SandboxPrepaySecs
	if prepaySecs <= 0 {
		prepaySecs = 30
	}
	triggerSecs := cfg.SandboxTopupTriggerSecs
	if triggerSecs <= 0 {
		triggerSecs = 5
	}

	topThresh := int64(triggerSecs) * ratePerSecond
	topAmt := int64(prepaySecs) * ratePerSecond

	return map[string]string{
		"SANDBOX_ID":             sb.ID.String(),
		"CONTROL_PLANE_URL":      cp,
		"JWT_SECRET":             string(cfg.WSSJWTSecret),
		"JWT_AUDIENCE":           cfg.DataJWTAudience,
		"SANDBOX_ROOT":           cfg.RuntimeSandboxRoot,
		"CALLBACK_SECRET":        cfg.CallbackSecret,
		"BILLING_METHOD":         sb.BillingMethod,
		"RATE_PER_SECOND":        fmt.Sprintf("%d", ratePerSecond),
		"INITIAL_BALANCE":        fmt.Sprintf("%d", initialBalance),
		"TOPUP_THRESHOLD":        fmt.Sprintf("%d", topThresh),
		"TOPUP_AMOUNT":           fmt.Sprintf("%d", topAmt),
		"BALANCE_LOW_WATER_SECS": fmt.Sprintf("%d", triggerSecs),
	}
}

// SandboxDepositObserver returns a payment.SandboxDepositObserver that reacts to a sandbox-scoped
// settlement: launches RunPod for an awaiting_payment sandbox, or resumes a paused-out-of-balance one.
// When the pod is already running the observer is a no-op (the runtime calls topup-trigger on the
// next tick and picks up the freshly-credited balance).
func SandboxDepositObserver(orch *Service, st *store.Store, cfg *config.Control) func(ctx context.Context, sandboxID uuid.UUID, amountMsats, newBalanceMsats int64) {
	return func(ctx context.Context, sandboxID uuid.UUID, amountMsats, newBalanceMsats int64) {
		if orch == nil || st == nil || cfg == nil {
			return
		}
		sb, err := st.GetSandbox(ctx, sandboxID)
		if err != nil || sb == nil {
			slog.Warn("settle observer: sandbox lookup", "sandbox", sandboxID, "err", err)
			return
		}
		pr, err := st.GetTierPricing(ctx, sb.Tier)
		if err != nil || pr == nil {
			slog.Warn("settle observer: tier pricing", "sandbox", sandboxID, "tier", sb.Tier, "err", err)
			return
		}

		switch strings.ToLower(strings.TrimSpace(sb.Status)) {
		case "awaiting_payment":
			env := BuildSandboxEnv(cfg, sb, newBalanceMsats, pr.MsatsPerSecond)
			if err := st.UpdateSandboxStatus(ctx, sb.ID, "pending"); err != nil {
				slog.Warn("settle observer: status pending", "sandbox", sandboxID, "err", err)
				return
			}
			if err := orch.CreateSandbox(ctx, sb, env); err != nil {
				slog.Warn("settle observer: orch.CreateSandbox", "sandbox", sandboxID, "err", err)
				_ = st.UpdateSandboxStatus(ctx, sb.ID, "awaiting_payment")
				return
			}
		case "paused":
			podID := ""
			if sb.PodID != nil {
				podID = *sb.PodID
			}
			if podID == "" {
				return
			}
			if err := orch.ResumeSandbox(ctx, podID, sb.ID); err != nil {
				slog.Warn("settle observer: orch.ResumeSandbox", "sandbox", sandboxID, "err", err)
			}
		}
	}
}
