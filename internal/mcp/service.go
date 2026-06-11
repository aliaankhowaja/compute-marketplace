package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
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


// Service is the MCP tool implementation.
type Service struct {
	Cfg        *config.Control
	Store      *store.Store
	PaySvc     *payment.Service
	Orch       *orchestrator.Service
	HTTPClient *http.Client
	Sessions   *SessionRegistry
	Executions *ExecutionRegistry
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

// ServeToolCall handles a tools/call request and writes a standard JSON-RPC response.
func (s *Service) ServeToolCall(w http.ResponseWriter, r *http.Request, session *Session, p *auth.Principal, name string, args json.RawMessage, rpcID json.RawMessage) {
	var res any
	var st int
	var slug string
	var extra map[string]any

	asyncTools := map[string]bool{"sandbox_exec": true, "sandbox_exec_cancel": true, "sandbox_exec_result": true}
	if asyncTools[name] {
		res, st, slug, extra = s.CallToolAsync(r.Context(), session, p, name, args)
	} else {
		res, st, slug, extra = s.CallTool(r.Context(), p, name, args)
	}

	var resp Response
	switch {
	case st == 0 && slug == "" && res == nil:
		resp = rpcErr(rpcID, http.StatusBadRequest, "unknown_tool", "unknown tool", map[string]any{"tool": name})
	case st != 0:
		msg := rpcErrMessageFromExtra(slug, extra)
		resp = rpcOK(rpcID, toolErrorResult(slug, msg, extra))
	default:
		resp = rpcOK(rpcID, rewrapToolResult(res))
	}

	writeJSON(w, http.StatusOK, resp)
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
	case "sandbox_create", "sandbox_list_tiers", "sandbox_exec", "sandbox_write_file", "sandbox_read_file", "sandbox_list_files",
		"sandbox_status", "sandbox_pause", "sandbox_resume", "sandbox_terminate", "sandbox_exec_cancel", "sandbox_exec_result":
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
		return nil, http.StatusServiceUnavailable, "store_unavailable", map[string]any{"message": "store not configured", "retryable": true, "hint": "Database temporarily unavailable. Retry in a few seconds."}
	}

	switch name {
	case "sandbox_create":
		return s.toolSandboxCreate(ctx, p, m)
	case "sandbox_list_tiers":
		return s.toolListTiers(ctx)
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
	case "sandbox_exec_cancel", "sandbox_exec_result":
		return nil, http.StatusBadRequest, "session_required", map[string]any{
			"message": fmt.Sprintf("%s requires an active session (Mcp-Session-Id header)", name),
			"hint":    "Call initialize first to obtain a session, then include Mcp-Session-Id header on subsequent requests.",
		}
	default:
		return nil, 0, "", nil
	}
}

// CallerKeyFromPrincipal derives a stable key for execution ownership tracking.
// Used by REST endpoints as an alternative to MCP session IDs.
func CallerKeyFromPrincipal(p *auth.Principal) string {
	if p == nil {
		return ""
	}
	switch p.Kind {
	case auth.KindAPIKey:
		if p.APIKeyID != nil {
			return "apikey:" + p.APIKeyID.String()
		}
	case auth.KindSandbox:
		if p.SandboxID != nil {
			return "sandbox:" + p.SandboxID.String()
		}
	}
	return ""
}

// CallerKeyFromToken derives a caller key from a sandbox_token string.
// Used when the caller authenticates via sandbox_token in the request body/query
// rather than the Authorization header.
func CallerKeyFromToken(token string) string {
	if auth.IsSandboxToken(token) {
		return "token:" + token
	}
	return ""
}

// CallToolAsync handles tools that return immediately and deliver results via SSE.
func (s *Service) CallToolAsync(ctx context.Context, session *Session, p *auth.Principal, name string, args json.RawMessage) (result any, httpStatus int, slug string, extra map[string]any) {
	if s == nil {
		return nil, http.StatusServiceUnavailable, "service_unavailable", map[string]any{"message": "service not configured"}
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

	switch name {
	case "sandbox_exec":
		id, ok := parseUUIDArg(m, "sandbox_id")
		if !ok {
			return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "sandbox_id"}
		}
		sb, st, slug, ex := s.getSandboxForPrincipal(ctx, p, id, m)
		if sb == nil {
			return nil, st, slug, ex
		}
		return s.toolExecAsyncWithKey(session.ID, sb, m)

	case "sandbox_exec_cancel":
		execID := strings.TrimSpace(argString(m, "execution_id"))
		if execID == "" {
			return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "execution_id required"}
		}
		if s.Executions == nil {
			return nil, http.StatusServiceUnavailable, "service_unavailable", nil
		}
		if ok := s.Executions.Cancel(execID); !ok {
			return nil, http.StatusNotFound, "not_found", map[string]any{
				"message": "execution not found or already completed",
				"hint":    "The execution may have already finished or been cleaned up.",
			}
		}
		return map[string]any{"execution_id": execID, "status": "cancelled"}, 0, "", nil

	case "sandbox_exec_result":
		execID := strings.TrimSpace(argString(m, "execution_id"))
		if execID == "" {
			return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "execution_id required"}
		}
		if s.Executions == nil {
			return nil, http.StatusServiceUnavailable, "service_unavailable", nil
		}
		exec := s.Executions.Get(execID)
		if exec == nil {
			return nil, http.StatusNotFound, "not_found", map[string]any{
				"message": "execution not found",
				"hint":    "The execution may have expired or been cleaned up. Completed executions are only retained temporarily.",
			}
		}
		if exec.SessionID != session.ID {
			return nil, http.StatusForbidden, "forbidden", map[string]any{
				"message": "execution belongs to another session",
				"hint":    "You can only poll executions started in your current session (same Mcp-Session-Id).",
			}
		}
		return s.toolExecResult(ctx, exec)

	default:
		return nil, 0, "", nil
	}
}

func (s *Service) toolExecResult(ctx context.Context, exec *Execution) (any, int, string, map[string]any) {
	chunks, status, exitCode, errMsg := exec.PollOutput()

	output := make([]map[string]any, 0, len(chunks))
	for _, c := range chunks {
		output = append(output, map[string]any{"stream": c.Stream, "chunk": c.Data})
	}

	out := map[string]any{
		"execution_id": exec.ID,
		"status":       status,
		"output":       output,
	}
	if exitCode != nil {
		out["exit_code"] = *exitCode
	}
	if errMsg != "" {
		out["error"] = errMsg
	}

	// Include pending invoice if sandbox has one
	if s.Store != nil {
		sb, err := s.Store.GetSandbox(ctx, exec.SandboxID)
		if err == nil && sb != nil && sb.BillingMethod == "lightning" && sb.PendingInvoiceBolt11 != nil {
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
	}

	return out, 0, "", nil
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
		return nil, http.StatusServiceUnavailable, "orchestrator_disabled", map[string]any{"message": err.Error(), "retryable": false, "hint": "Pod orchestration not configured on this server."}
	}

	switch bm {
	case "credits":
		initialCents := int64(argFloat(args, "initial_credits_cents"))
		return s.createCreditsSandbox(ctx, p, tier, templateLabel, runtimeLabel, pr, initialCents)
	case "lightning":
		clientPrepaidSats := int64(argFloat(args, "prepaid_sats"))
		return s.createLightningSandbox(ctx, tier, templateLabel, runtimeLabel, pr, clientPrepaidSats)
	}
	return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "billing_method"}
}

func (s *Service) createCreditsSandbox(ctx context.Context, p *auth.Principal, tier, templateLabel, runtimeLabel string, pr *store.TierPricing, initialCents int64) (any, int, string, map[string]any) {
	if p == nil || p.Kind != auth.KindAPIKey || p.UserID == nil || p.APIKeyID == nil {
		return nil, http.StatusUnauthorized, "unauthorized", map[string]any{"message": "credits billing requires an API key (Bearer sockt_live_...)"}
	}

	minSubcents := pr.USDCentsPerSecondX100 * 3600
	if minSubcents <= 0 {
		minSubcents = store.SubcentsPerCent
	}
	allocRequest := initialCents * store.SubcentsPerCent
	if allocRequest <= 0 {
		allocRequest = minSubcents
	}
	if allocRequest < minSubcents {
		return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": fmt.Sprintf("minimum allocation is %d cents (1 hour of compute)", minSubcents/store.SubcentsPerCent)}
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
	alloc, _, _, err := s.Store.AllocateCreditsForSandbox(ctx, *sb.UserID, sb.ID, allocRequest)
	if err != nil {
		return nil, http.StatusPaymentRequired, "insufficient_credits", map[string]any{"message": err.Error()}
	}
	rate := pr.USDCentsPerSecondX100
	if rate <= 0 {
		rate = 1
	}
	env := orchestrator.BuildSandboxEnv(s.Cfg, sb, alloc, rate)
	if s.Orch == nil {
		return nil, http.StatusServiceUnavailable, "orchestrator_disabled", map[string]any{"message": "sandbox orchestration not configured", "retryable": false, "hint": "Sandbox provisioning is not configured on this server."}
	}
	if err := s.Orch.CreateSandbox(ctx, sb, env); err != nil {
		_, _ = s.Store.RefundSandboxCredits(ctx, *sb.UserID, sb.ID)
		return nil, http.StatusBadGateway, "provisioning_error", map[string]any{"message": err.Error()}
	}
	sb2, _ := s.Store.GetSandbox(ctx, sb.ID)
	return sb2, 0, "", nil
}

func (s *Service) createLightningSandbox(ctx context.Context, tier, templateLabel, runtimeLabel string, pr *store.TierPricing, clientPrepaidSats int64) (any, int, string, map[string]any) {
	if s.PaySvc == nil || !s.PaySvc.HasLightning() {
		return nil, http.StatusServiceUnavailable, "payment_disabled", map[string]any{"message": "lightning payment not configured", "retryable": false, "hint": "Lightning payments not configured on this server."}
	}
	rate := pr.MsatsPerSecond
	if rate <= 0 {
		return nil, http.StatusInternalServerError, "tier_error", map[string]any{"message": "tier has no lightning rate"}
	}

	var prepaySats int64
	var prepaySecs int
	if clientPrepaidSats > 0 {
		prepaySats = clientPrepaidSats
		prepaySecs = int((prepaySats * 1000) / rate)
	} else {
		prepaySecs = s.Cfg.SandboxPrepaySecs
		if prepaySecs <= 0 {
			prepaySecs = 30
		}
		prepayMsats := int64(prepaySecs) * rate
		prepaySats = prepayMsats / 1000
		if prepayMsats%1000 != 0 {
			prepaySats++
		}
		if prepaySats <= 0 {
			prepaySats = 1
		}
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
		"runtime":           publicRuntime(runtimeLabel),
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
		"runtime":        publicRuntime(sb.Runtime),
		"billing_method": sb.BillingMethod,
		"template":       sb.Template,
		"consumed_msats": sb.ConsumedMsats,
		"metrics_stale":  true,
	}

	pr, _ := s.Store.GetTierPricing(ctx, sb.Tier)
	if pr != nil {
		out["msats_per_second"] = pr.MsatsPerSecond
		if pr.MsatsPerSecond > 0 {
			out["sats_per_second"] = float64(pr.MsatsPerSecond) / 1000.0
		}
	}

	if sb.BillingMethod == "lightning" {
		out["prepaid_balance_msats"] = sb.PrepaidBalanceMsats

		if sb.Status == "pending" {
			out["warning_level"] = "provisioning"
			out["next_poll_secs"] = int64(5)
		} else {
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

			var nextPoll int64
			switch out["warning_level"] {
			case "awaiting_payment", "out_of_balance":
				nextPoll = 5
			default:
				timeUntilTrigger := secsRemaining - int64(s.Cfg.SandboxTopupTriggerSecs)
				if timeUntilTrigger < 0 {
					timeUntilTrigger = 0
				}
				nextPoll = timeUntilTrigger / 2
				if nextPoll < 5 {
					nextPoll = 5
				}
				if nextPoll > 60 {
					nextPoll = 60
				}
			}
			out["next_poll_secs"] = nextPoll
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
		return nil, http.StatusServiceUnavailable, "orchestrator_disabled", map[string]any{"message": "sandbox orchestration not available", "retryable": false, "hint": "Sandbox provisioning is not configured on this server."}
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
		return nil, http.StatusServiceUnavailable, "orchestrator_disabled", map[string]any{"message": "sandbox orchestration not available", "retryable": false, "hint": "Sandbox provisioning is not configured on this server."}
	}
	if err := s.Orch.ResumeSandbox(ctx, podID, id, sb.Tier); err != nil {
		return nil, http.StatusBadGateway, "resume_failed", map[string]any{"message": err.Error()}
	}
	out := map[string]any{"sandbox_id": id.String(), "status": "running"}
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
		return nil, http.StatusServiceUnavailable, "orchestrator_disabled", map[string]any{"message": "sandbox orchestration not available", "retryable": false, "hint": "Sandbox provisioning is not configured on this server."}
	}
	if err := s.Orch.TerminateSandbox(ctx, podID, id); err != nil {
		return nil, http.StatusBadGateway, "terminate_failed", map[string]any{"message": err.Error()}
	}

	var refundResult map[string]any
	lightningAddr := strings.TrimSpace(argString(args, "lightning_address"))
	if sb.BillingMethod == "lightning" && lightningAddr != "" && sb.PrepaidBalanceMsats > 0 {
		refundResult = s.attemptLightningRefund(ctx, id, sb.PrepaidBalanceMsats, lightningAddr)
	}

	if sb.BillingMethod == "credits" && sb.UserID != nil {
		refundedSubcents, refundErr := s.Store.RefundSandboxCredits(ctx, *sb.UserID, sb.ID)
		if refundErr != nil {
			slog.Error("credit refund on terminate failed", "sandbox_id", id, "err", refundErr)
		}
		if refundedSubcents > 0 {
			refundResult = map[string]any{"status": "credited", "amount_cents": refundedSubcents / store.SubcentsPerCent}
		}
	}

	out := map[string]any{
		"sandbox_id":           id.String(),
		"status":               "terminated",
		"consumed_msats_total": sb.ConsumedMsats,
		"terminated_at":        time.Now().UTC().Format(time.RFC3339),
	}
	if refundResult != nil {
		out["refund"] = refundResult
	}
	return out, 0, "", nil
}

func (s *Service) attemptLightningRefund(ctx context.Context, sandboxID uuid.UUID, remainingMsats int64, address string) map[string]any {
	if s.PaySvc == nil || !s.PaySvc.HasLightning() {
		slog.Warn("refund skipped: lightning not configured", "sandbox_id", sandboxID.String())
		return map[string]any{"status": "skipped", "reason": "lightning not configured"}
	}

	refundSats := remainingMsats / 1000
	if remainingMsats%1000 != 0 {
		refundSats++
	}
	if refundSats <= 0 {
		return map[string]any{"status": "skipped", "reason": "balance too small"}
	}

	idem := fmt.Sprintf("sandbox-refund:%s", sandboxID.String())
	if err := s.PaySvc.SendToAddress(ctx, address, refundSats, idem); err != nil {
		slog.Error("lightning refund failed", "sandbox_id", sandboxID.String(), "address", address, "amount_sats", refundSats, "err", err)
		return map[string]any{"status": "failed", "reason": err.Error(), "amount_sats": refundSats}
	}

	slog.Info("lightning refund sent", "sandbox_id", sandboxID.String(), "address", address, "amount_sats", refundSats)
	return map[string]any{"status": "sent", "amount_sats": refundSats, "address": address}
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

func (s *Service) toolListTiers(ctx context.Context) (any, int, string, map[string]any) {
	tiers, err := s.Store.ListTierPricing(ctx)
	if err != nil {
		return nil, http.StatusInternalServerError, "store_error", map[string]any{"message": err.Error()}
	}

	result := make([]map[string]any, 0, len(tiers))
	for _, t := range tiers {
		tierInfo := map[string]any{
			"tier":              t.Tier,
			"msats_per_second":  t.MsatsPerSecond,
		}

		// Calculate sats per second (from msats)
		if t.MsatsPerSecond > 0 {
			tierInfo["sats_per_second"] = float64(t.MsatsPerSecond) / 1000.0
		}

		if t.USDCentsPerSecondX100 > 0 {
			tierInfo["usd_cents_per_second"] = float64(t.USDCentsPerSecondX100) / 100000.0
		}

		// Add examples for common durations
		if t.MsatsPerSecond > 0 {
			// Cost for 1 minute, 1 hour, 1 day in sats
			tierInfo["cost_per_minute_sats"] = float64(t.MsatsPerSecond*60) / 1000.0
			tierInfo["cost_per_hour_sats"] = float64(t.MsatsPerSecond*3600) / 1000.0
			tierInfo["cost_per_day_sats"] = float64(t.MsatsPerSecond*86400) / 1000.0
		}

		if t.USDCentsPerSecondX100 > 0 {
			tierInfo["cost_per_hour_usd"] = float64(t.USDCentsPerSecondX100*3600) / (100000.0 * 100.0)
		}

		result = append(result, tierInfo)
	}

	return map[string]any{
		"tiers": result,
		"note":  "Lightning billing uses msats_per_second and sats_per_second rates. Credits billing uses usd_cents_per_second rates.",
	}, 0, "", nil
}

// CallToolAsyncREST handles async exec tools for the REST API surface.
// It uses callerKey (derived from principal) instead of an MCP session ID.
func (s *Service) CallToolAsyncREST(ctx context.Context, callerKey string, p *auth.Principal, name string, args json.RawMessage) (result any, httpStatus int, slug string, extra map[string]any) {
	if s == nil {
		return nil, http.StatusServiceUnavailable, "service_unavailable", map[string]any{"message": "service not configured"}
	}
	if callerKey == "" {
		return nil, http.StatusUnauthorized, "unauthorized", map[string]any{"message": "authentication required for exec operations"}
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

	switch name {
	case "sandbox_exec":
		id, ok := parseUUIDArg(m, "sandbox_id")
		if !ok {
			return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "sandbox_id"}
		}
		sb, st, slug, ex := s.getSandboxForPrincipal(ctx, p, id, m)
		if sb == nil {
			return nil, st, slug, ex
		}
		return s.toolExecAsyncWithKey(callerKey, sb, m)

	case "sandbox_exec_cancel":
		execID := strings.TrimSpace(argString(m, "execution_id"))
		if execID == "" {
			return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "execution_id required"}
		}
		if s.Executions == nil {
			return nil, http.StatusServiceUnavailable, "service_unavailable", nil
		}
		exec := s.Executions.Get(execID)
		if exec == nil {
			return nil, http.StatusNotFound, "not_found", map[string]any{
				"message": "execution not found or already completed",
				"hint":    "The execution may have already finished or been cleaned up.",
			}
		}
		if exec.SessionID != callerKey {
			return nil, http.StatusForbidden, "forbidden", map[string]any{
				"message": "execution belongs to another caller",
			}
		}
		if ok := s.Executions.Cancel(execID); !ok {
			return nil, http.StatusNotFound, "not_found", map[string]any{
				"message": "execution not found or already completed",
			}
		}
		return map[string]any{"execution_id": execID, "status": "cancelled"}, 0, "", nil

	case "sandbox_exec_result":
		execID := strings.TrimSpace(argString(m, "execution_id"))
		if execID == "" {
			return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "execution_id required"}
		}
		if s.Executions == nil {
			return nil, http.StatusServiceUnavailable, "service_unavailable", nil
		}
		exec := s.Executions.Get(execID)
		if exec == nil {
			return nil, http.StatusNotFound, "not_found", map[string]any{
				"message": "execution not found",
				"hint":    "The execution may have expired or been cleaned up. Completed executions are only retained temporarily.",
			}
		}
		if exec.SessionID != callerKey {
			return nil, http.StatusForbidden, "forbidden", map[string]any{
				"message": "execution belongs to another caller",
			}
		}
		return s.toolExecResult(ctx, exec)

	default:
		return nil, 0, "", nil
	}
}

// ShellURL returns an authenticated WebSocket URL for the sandbox shell.
func (s *Service) ShellURL(ctx context.Context, p *auth.Principal, rawArgs json.RawMessage) (any, int, string, map[string]any) {
	var args map[string]any
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, http.StatusBadRequest, "bad_request", map[string]any{"message": "invalid JSON"}
	}
	id, ok := parseUUIDArg(args, "sandbox_id")
	if !ok {
		return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "sandbox_id"}
	}
	sb, st, slug, ex := s.getSandboxForPrincipal(ctx, p, id, args)
	if sb == nil {
		return nil, st, slug, ex
	}
	base := dataPlaneBase(sb)
	if base == "" {
		return nil, http.StatusBadRequest, "pod_unreachable", map[string]any{"message": "no data plane endpoint"}
	}
	tok, err := s.proxyDataJWT(sb, "shell")
	if err != nil {
		return nil, http.StatusInternalServerError, "token_error", map[string]any{"message": err.Error()}
	}
	host := strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
	wsURL := fmt.Sprintf("wss://%s/sandbox/%s/shell?token=%s", host, sb.ID.String(), tok)
	ttl := s.Cfg.DataJWTTTL
	if s.Cfg.MCPDataJWTTTL > 0 {
		ttl = s.Cfg.MCPDataJWTTTL
	}
	expiresAt := time.Now().Add(ttl).UTC().Format(time.RFC3339)
	return map[string]any{"url": wsURL, "expires_at": expiresAt}, 0, "", nil
}
