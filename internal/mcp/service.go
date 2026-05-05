package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aliaankhowaja/sockt-backend/internal/auth"
	"github.com/aliaankhowaja/sockt-backend/internal/config"
	"github.com/aliaankhowaja/sockt-backend/internal/orchestrator"
	"github.com/aliaankhowaja/sockt-backend/internal/payment"
	"github.com/aliaankhowaja/sockt-backend/internal/store"
)

// Service is the MCP tool implementation (stateless).
type Service struct {
	Cfg        *config.Control
	Store      *store.Store
	PaySvc     *payment.Service
	Orch       *orchestrator.Service
	HTTPClient *http.Client
}

// NewService constructs an MCP service. HTTPClient may be nil (default from config timeout).
func NewService(cfg *config.Control, st *store.Store, paySvc *payment.Service, orch *orchestrator.Service, hc *http.Client) *Service {
	return &Service{
		Cfg:        cfg,
		Store:      st,
		PaySvc:     paySvc,
		Orch:       orch,
		HTTPClient: hc,
	}
}

func (s *Service) principalCanAccess(_ context.Context, p *auth.Principal, sb *store.Sandbox, args map[string]any) (bool, error) {
	if sb == nil {
		return false, nil
	}
	if tok := strings.TrimSpace(argString(args, "sandbox_token")); auth.IsSandboxToken(tok) {
		if sb.SandboxToken != nil && *sb.SandboxToken == tok {
			return true, nil
		}
	}
	if p == nil {
		return false, nil
	}
	switch p.Kind {
	case auth.KindAPIKey:
		if p.UserID != nil && sb.UserID != nil {
			return *p.UserID == *sb.UserID, nil
		}
	case auth.KindSandbox:
		if p.SandboxID != nil {
			return *p.SandboxID == sb.ID, nil
		}
	}
	return false, nil
}

// CallTool runs a named tool. httpStatus 0 with empty slug means unknown tool. On success httpStatus is 0 and slug is "".
func (s *Service) CallTool(ctx context.Context, p *auth.Principal, name string, args json.RawMessage) (result any, httpStatus int, slug string, extra map[string]any) {
	if s == nil {
		return nil, http.StatusServiceUnavailable, "service_unavailable", map[string]any{"message": "service not configured"}
	}

	name = strings.TrimSpace(name)
	switch name {
	case "":
		return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "tool name required"}
	case "sandbox_create", "sandbox_exec", "sandbox_write_file", "sandbox_read_file", "sandbox_list_files",
		"sandbox_status", "sandbox_pause", "sandbox_resume", "sandbox_terminate":
	default:
		return nil, 0, "", nil
	}

	var m map[string]any
	if len(args) > 0 && string(args) != "null" {
		if err := json.Unmarshal(args, &m); err != nil {
			return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "arguments must be a JSON object"}
		}
	}
	if m == nil {
		m = map[string]any{}
	}

	if s.Store == nil {
		return nil, http.StatusServiceUnavailable, "store_unavailable", map[string]any{"message": "store not configured"}
	}

	switch name {
	case "sandbox_create":
		return s.toolSandboxCreate(ctx, p, m)
	case "sandbox_exec":
		return s.toolSandboxExec(ctx, p, m)
	case "sandbox_write_file":
		return s.toolSandboxWriteFile(ctx, p, m)
	case "sandbox_read_file":
		return s.toolSandboxReadFile(ctx, p, m)
	case "sandbox_list_files":
		return s.toolSandboxListFiles(ctx, p, m)
	case "sandbox_status":
		return s.toolSandboxStatus(ctx, p, m)
	case "sandbox_pause":
		return s.toolSandboxPause(ctx, p, m)
	case "sandbox_resume":
		return s.toolSandboxResume(ctx, p, m)
	case "sandbox_terminate":
		return s.toolSandboxTerminate(ctx, p, m)
	default:
		return nil, 0, "", nil
	}
}

func (s *Service) getSandboxForPrincipal(ctx context.Context, p *auth.Principal, id uuid.UUID, args map[string]any) (*store.Sandbox, int, string, map[string]any) {
	sb, err := s.Store.GetSandbox(ctx, id)
	if err != nil {
		return nil, http.StatusInternalServerError, "store_error", map[string]any{"message": err.Error()}
	}
	if sb == nil {
		return nil, http.StatusNotFound, "not_found", map[string]any{"sandbox_id": id.String()}
	}
	ok, err := s.principalCanAccess(ctx, p, sb, args)
	if err != nil {
		return nil, http.StatusInternalServerError, "store_error", map[string]any{"message": err.Error()}
	}
	if !ok {
		return nil, http.StatusForbidden, "forbidden", map[string]any{"sandbox_id": id.String()}
	}
	return sb, 0, "", nil
}

func (s *Service) toolSandboxCreate(ctx context.Context, p *auth.Principal, args map[string]any) (any, int, string, map[string]any) {
	bm := strings.ToLower(strings.TrimSpace(argString(args, "billing_method")))
	if bm == "" {
		// Default to lightning for credential-less callers (autonomous LLM agents); credits otherwise.
		if p != nil && p.Kind == auth.KindAPIKey {
			bm = "credits"
		} else {
			bm = "lightning"
		}
	}
	if bm != "lightning" && bm != "credits" {
		return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "billing_method must be lightning or credits"}
	}

	tier := strings.TrimSpace(argString(args, "tier"))
	if tier == "" {
		return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "tier required"}
	}
	pr, err := s.Store.GetTierPricing(ctx, tier)
	if err != nil || pr == nil {
		return nil, http.StatusBadRequest, "bad_tier", map[string]any{"message": "unknown tier"}
	}
	_, templateLabel, runtimeLabel, err := orchestrator.ResolveTier(s.Cfg, tier)
	if err != nil {
		return nil, http.StatusServiceUnavailable, "orchestrator_disabled", map[string]any{"message": err.Error()}
	}

	switch bm {
	case "credits":
		return s.createCreditsSandbox(ctx, p, tier, templateLabel, runtimeLabel, pr)
	case "lightning":
		return s.createLightningSandbox(ctx, tier, templateLabel, runtimeLabel, pr)
	}
	return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "billing_method"}
}

func (s *Service) createCreditsSandbox(ctx context.Context, p *auth.Principal, tier, templateLabel, runtimeLabel string, pr *store.TierPricing) (any, int, string, map[string]any) {
	if p == nil || p.Kind != auth.KindAPIKey || p.UserID == nil || p.APIKeyID == nil {
		return nil, http.StatusUnauthorized, "unauthorized", map[string]any{"message": "credits billing requires an API key (Bearer sockt_live_...)"}
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
	if err := s.Store.InsertSandbox(ctx, sb); err != nil {
		return nil, http.StatusInternalServerError, "store_error", map[string]any{"message": err.Error()}
	}
	alloc, _, err := s.Store.AllocateCreditsForSandbox(ctx, *sb.UserID, sb.ID, s.Cfg.LowCreditThresholdCents*10)
	if err != nil {
		return nil, http.StatusPaymentRequired, "insufficient_credits", map[string]any{"message": err.Error()}
	}
	rate := int64(1)
	if pr.USDCentsPerSecondX100 > 0 {
		rate = pr.USDCentsPerSecondX100 / 100
		if rate <= 0 {
			rate = 1
		}
	}
	env := orchestrator.BuildSandboxEnv(s.Cfg, sb, alloc, rate)
	if s.Orch == nil {
		return nil, http.StatusServiceUnavailable, "orchestrator_disabled", map[string]any{"message": "runpod not configured"}
	}
	if err := s.Orch.CreateSandbox(ctx, sb, env); err != nil {
		return nil, http.StatusBadGateway, "runpod_error", map[string]any{"message": err.Error()}
	}
	sb2, _ := s.Store.GetSandbox(ctx, sb.ID)
	return sb2, 0, "", nil
}

func (s *Service) createLightningSandbox(ctx context.Context, tier, templateLabel, runtimeLabel string, pr *store.TierPricing) (any, int, string, map[string]any) {
	if s.PaySvc == nil || !s.PaySvc.HasLightning() {
		return nil, http.StatusServiceUnavailable, "payment_disabled", map[string]any{"message": "lightning payment not configured"}
	}
	prepaySecs := s.Cfg.SandboxPrepaySecs
	if prepaySecs <= 0 {
		prepaySecs = 30
	}
	rate := pr.MsatsPerSecond
	if rate <= 0 {
		return nil, http.StatusInternalServerError, "tier_error", map[string]any{"message": "tier has no lightning rate"}
	}
	prepayMsats := int64(prepaySecs) * rate
	prepaySats := prepayMsats / 1000
	if prepayMsats%1000 != 0 {
		prepaySats++
	}
	if prepaySats <= 0 {
		prepaySats = 1
	}

	token, err := newSandboxToken()
	if err != nil {
		return nil, http.StatusInternalServerError, "internal", map[string]any{"message": err.Error()}
	}

	sb := &store.Sandbox{
		BillingMethod: "lightning",
		Tier:          tier,
		Template:      templateLabel,
		Status:        "awaiting_payment",
		Runtime:       runtimeLabel,
		SandboxToken:  &token,
	}
	if err := s.Store.InsertSandbox(ctx, sb); err != nil {
		return nil, http.StatusInternalServerError, "store_error", map[string]any{"message": err.Error()}
	}

	memo := fmt.Sprintf("sockt-prepay:sandbox:%s", sb.ID.String())
	idem := fmt.Sprintf("sandbox-prepay:%s", sb.ID.String())
	dep, err := s.PaySvc.RequestSandboxDepositInvoice(ctx, sb.ID, prepaySats, 3600, memo, idem)
	if err != nil || dep == nil {
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		return nil, http.StatusBadGateway, "invoice_failed", map[string]any{"message": msg}
	}
	if len(dep.RHash) != 32 {
		return nil, http.StatusBadGateway, "invoice_bad_hash", nil
	}
	expiresAt := time.Now().UTC().Add(time.Duration(dep.ExpirySeconds) * time.Second)
	if err := s.Store.SetSandboxPendingInvoice(ctx, sb.ID, dep.PaymentRequest, dep.RHash, dep.AmountMsats, expiresAt); err != nil {
		return nil, http.StatusInternalServerError, "store_error", map[string]any{"message": err.Error()}
	}

	out := map[string]any{
		"status":            "awaiting_payment",
		"sandbox_id":        sb.ID.String(),
		"sandbox_token":     token,
		"tier":              tier,
		"runtime":           runtimeLabel,
		"billing_method":    "lightning",
		"invoice":           dep.PaymentRequest,
		"payment_hash":      hex.EncodeToString(dep.RHash),
		"amount_msats":      dep.AmountMsats,
		"amount_sats":       prepaySats,
		"msats_per_second":  rate,
		"prepaid_seconds":   prepaySecs,
		"expires_at":        expiresAt.Format(time.RFC3339),
		"next_step":         fmt.Sprintf("Pay this BOLT11. Then call sandbox_status with sandbox_id=%s and sandbox_token to track readiness.", sb.ID.String()),
		"auth_hint":         "Send Authorization: Bearer " + token + " on every subsequent MCP POST, or pass sandbox_token as a tool argument.",
	}
	return out, 0, "", nil
}

func (s *Service) toolSandboxExec(ctx context.Context, p *auth.Principal, args map[string]any) (any, int, string, map[string]any) {
	id, ok := parseUUIDArg(args, "sandbox_id")
	if !ok {
		return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "sandbox_id"}
	}
	sb, st, slug, ex := s.getSandboxForPrincipal(ctx, p, id, args)
	if sb == nil {
		return nil, st, slug, ex
	}
	return s.toolExec(ctx, sb, args)
}

func (s *Service) toolSandboxWriteFile(ctx context.Context, p *auth.Principal, args map[string]any) (any, int, string, map[string]any) {
	id, ok := parseUUIDArg(args, "sandbox_id")
	if !ok {
		return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "sandbox_id"}
	}
	sb, st, slug, ex := s.getSandboxForPrincipal(ctx, p, id, args)
	if sb == nil {
		return nil, st, slug, ex
	}
	return s.toolWriteFile(ctx, sb, args)
}

func (s *Service) toolSandboxReadFile(ctx context.Context, p *auth.Principal, args map[string]any) (any, int, string, map[string]any) {
	id, ok := parseUUIDArg(args, "sandbox_id")
	if !ok {
		return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "sandbox_id"}
	}
	sb, st, slug, ex := s.getSandboxForPrincipal(ctx, p, id, args)
	if sb == nil {
		return nil, st, slug, ex
	}
	return s.toolReadFile(ctx, sb, args)
}

func (s *Service) toolSandboxListFiles(ctx context.Context, p *auth.Principal, args map[string]any) (any, int, string, map[string]any) {
	id, ok := parseUUIDArg(args, "sandbox_id")
	if !ok {
		return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "sandbox_id"}
	}
	sb, st, slug, ex := s.getSandboxForPrincipal(ctx, p, id, args)
	if sb == nil {
		return nil, st, slug, ex
	}
	return s.toolListFiles(ctx, sb, args)
}

func (s *Service) toolSandboxStatus(ctx context.Context, p *auth.Principal, args map[string]any) (any, int, string, map[string]any) {
	id, ok := parseUUIDArg(args, "sandbox_id")
	if !ok {
		return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "sandbox_id"}
	}
	sb, st, slug, ex := s.getSandboxForPrincipal(ctx, p, id, args)
	if sb == nil {
		return nil, st, slug, ex
	}

	out := map[string]any{
		"sandbox_id":     sb.ID.String(),
		"status":         sb.Status,
		"tier":           sb.Tier,
		"runtime":        sb.Runtime,
		"billing_method": sb.BillingMethod,
		"template":       sb.Template,
		"consumed_msats": sb.ConsumedMsats,
		"metrics_stale":  true,
		"shell_url":      "",
		"data_endpoint":  "",
	}
	if sb.PodID != nil {
		out["pod_id"] = *sb.PodID
		ph := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(dataPlaneBase(sb), "https://"), "http://"))
		if ph != "" {
			out["shell_url"] = fmt.Sprintf("wss://%s/sandbox/%s/shell", ph, sb.ID.String())
		}
	}
	out["data_endpoint"] = dataPlaneBase(sb)

	pr, _ := s.Store.GetTierPricing(ctx, sb.Tier)
	if pr != nil {
		out["msats_per_second"] = pr.MsatsPerSecond
		if pr.MsatsPerSecond > 0 {
			out["sats_per_second"] = float64(pr.MsatsPerSecond) / 1000.0
		}
	}

	if sb.BillingMethod == "lightning" {
		out["prepaid_balance_msats"] = sb.PrepaidBalanceMsats
		secsRemaining := int64(0)
		if pr != nil && pr.MsatsPerSecond > 0 {
			secsRemaining = sb.PrepaidBalanceMsats / pr.MsatsPerSecond
			out["seconds_remaining"] = secsRemaining
		}
		switch {
		case sb.Status == "awaiting_payment":
			out["warning_level"] = "awaiting_payment"
		case secsRemaining <= 0 && sb.PrepaidBalanceMsats <= 0:
			out["warning_level"] = "out_of_balance"
		case secsRemaining < int64(s.Cfg.SandboxTopupTriggerSecs):
			out["warning_level"] = "low_balance"
		default:
			out["warning_level"] = "ok"
		}
		if sb.PendingInvoiceBolt11 != nil {
			pending := map[string]any{
				"bolt11": *sb.PendingInvoiceBolt11,
			}
			if len(sb.PendingInvoicePaymentHash) == 32 {
				pending["payment_hash"] = hex.EncodeToString(sb.PendingInvoicePaymentHash)
			}
			if sb.PendingInvoiceAmountMsats != nil {
				pending["amount_msats"] = *sb.PendingInvoiceAmountMsats
			}
			if sb.PendingInvoiceExpiresAt != nil {
				pending["expires_at"] = sb.PendingInvoiceExpiresAt.UTC().Format(time.RFC3339)
			}
			out["pending_invoice"] = pending
		}
		if sb.SandboxToken != nil {
			out["sandbox_token"] = *sb.SandboxToken
		}
	}

	live, _, _, _ := s.toolRuntimeStatus(ctx, sb)
	if live != nil && len(live) > 0 {
		out["metrics_stale"] = false
		for k, v := range live {
			out["runtime_"+k] = v
		}
		if bal, ok := live["balance"]; ok {
			out["balance_local"] = bal
		}
	}

	return out, 0, "", nil
}

func (s *Service) toolSandboxPause(ctx context.Context, p *auth.Principal, args map[string]any) (any, int, string, map[string]any) {
	id, ok := parseUUIDArg(args, "sandbox_id")
	if !ok {
		return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "sandbox_id"}
	}
	sb, st, slug, ex := s.getSandboxForPrincipal(ctx, p, id, args)
	if sb == nil {
		return nil, st, slug, ex
	}
	podID := ""
	if sb.PodID != nil {
		podID = *sb.PodID
	}
	if s.Orch == nil {
		return nil, http.StatusServiceUnavailable, "orchestrator_disabled", nil
	}
	if err := s.Orch.PauseSandbox(ctx, podID, id); err != nil {
		return nil, http.StatusBadGateway, "pause_failed", map[string]any{"message": err.Error()}
	}
	return map[string]any{"sandbox_id": id.String(), "status": "paused"}, 0, "", nil
}

func (s *Service) toolSandboxResume(ctx context.Context, p *auth.Principal, args map[string]any) (any, int, string, map[string]any) {
	id, ok := parseUUIDArg(args, "sandbox_id")
	if !ok {
		return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "sandbox_id"}
	}
	sb, st, slug, ex := s.getSandboxForPrincipal(ctx, p, id, args)
	if sb == nil {
		return nil, st, slug, ex
	}
	podID := ""
	if sb.PodID != nil {
		podID = *sb.PodID
	}
	if s.Orch == nil {
		return nil, http.StatusServiceUnavailable, "orchestrator_disabled", nil
	}
	if err := s.Orch.ResumeSandbox(ctx, podID, id); err != nil {
		return nil, http.StatusBadGateway, "resume_failed", map[string]any{"message": err.Error()}
	}
	sb2, _ := s.Store.GetSandbox(ctx, id)
	out := map[string]any{"sandbox_id": id.String(), "status": "running"}
	if sb2 != nil && sb2.PodProxyURL != nil {
		out["data_endpoint"] = strings.TrimRight(*sb2.PodProxyURL, "/")
	}
	if sb2 != nil && sb2.PodID != nil {
		out["host_id"] = *sb2.PodID
	}
	return out, 0, "", nil
}

func (s *Service) toolSandboxTerminate(ctx context.Context, p *auth.Principal, args map[string]any) (any, int, string, map[string]any) {
	id, ok := parseUUIDArg(args, "sandbox_id")
	if !ok {
		return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "sandbox_id"}
	}
	confirm := false
	if v, ok := args["confirm"].(bool); ok {
		confirm = v
	}
	if !confirm {
		return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "confirm must be true"}
	}
	sb, st, slug, ex := s.getSandboxForPrincipal(ctx, p, id, args)
	if sb == nil {
		return nil, st, slug, ex
	}
	podID := ""
	if sb.PodID != nil {
		podID = *sb.PodID
	}
	if s.Orch == nil {
		return nil, http.StatusServiceUnavailable, "orchestrator_disabled", nil
	}
	if err := s.Orch.TerminateSandbox(ctx, podID, id); err != nil {
		return nil, http.StatusBadGateway, "terminate_failed", map[string]any{"message": err.Error()}
	}
	return map[string]any{
		"sandbox_id":           id.String(),
		"status":               "terminated",
		"total_uptime_secs":    0,
		"consumed_msats_total": sb.ConsumedMsats,
		"terminated_at":        time.Now().UTC().Format(time.RFC3339),
	}, 0, "", nil
}

// newSandboxToken returns a 256-bit Crockford-base32 opaque bearer token, prefixed `sbx_`.
func newSandboxToken() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	out := make([]byte, len(raw)*2)
	for i, b := range raw {
		out[2*i] = alphabet[b>>4]
		out[2*i+1] = alphabet[b&0x0F]
	}
	return auth.SandboxTokenPrefix + strings.ToLower(string(out)), nil
}
