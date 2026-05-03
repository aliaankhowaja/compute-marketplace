package mcp

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aliaankhowaja/sockt-backend/internal/auth"
	"github.com/aliaankhowaja/sockt-backend/internal/config"
	"github.com/aliaankhowaja/sockt-backend/internal/lightningwallet"
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

func (s *Service) principalCanAccess(ctx context.Context, p *auth.Principal, sb *store.Sandbox) (bool, error) {
	if p == nil || sb == nil {
		return false, nil
	}
	switch p.Kind {
	case auth.KindAPIKey:
		if p.UserID != nil && sb.UserID != nil {
			return *p.UserID == *sb.UserID, nil
		}
	case auth.KindL402:
		if p.AgentID != nil && sb.AgentID != nil {
			return *p.AgentID == *sb.AgentID, nil
		}
	case auth.KindUserJWT:
		if sb.BillingMethod == "credits" && p.UserID != nil && sb.UserID != nil {
			return *p.UserID == *sb.UserID, nil
		}
		if sb.BillingMethod == "lightning" && p.UserID != nil && sb.AgentID != nil {
			ag, err := s.Store.GetAgentByJWTSub(ctx, *p.UserID)
			if err != nil {
				return false, err
			}
			if ag == nil {
				return false, nil
			}
			return ag.ID == *sb.AgentID, nil
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
		"sandbox_status", "sandbox_pause", "sandbox_resume", "sandbox_terminate", "sandbox_extend_balance",
		"wallet_balance", "wallet_deposit", "wallet_deposit_status":
		if s.Store == nil {
			return nil, http.StatusServiceUnavailable, "store_unavailable", map[string]any{"message": "store not configured"}
		}
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
	case "sandbox_extend_balance":
		return s.toolSandboxExtendBalance(ctx, p, m)
	case "wallet_balance":
		return s.toolWalletBalance(ctx, p)
	case "wallet_deposit":
		return s.toolWalletDeposit(ctx, p, m)
	case "wallet_deposit_status":
		return s.toolWalletDepositStatus(ctx, p, m)
	default:
		return nil, 0, "", nil
	}
}

func (s *Service) getSandboxForPrincipal(ctx context.Context, p *auth.Principal, id uuid.UUID) (*store.Sandbox, int, string, map[string]any) {
	sb, err := s.Store.GetSandbox(ctx, id)
	if err != nil {
		return nil, http.StatusInternalServerError, "store_error", map[string]any{"message": err.Error()}
	}
	if sb == nil {
		return nil, http.StatusNotFound, "not_found", map[string]any{"sandbox_id": id.String()}
	}
	ok, err := s.principalCanAccess(ctx, p, sb)
	if err != nil {
		return nil, http.StatusInternalServerError, "store_error", map[string]any{"message": err.Error()}
	}
	if !ok {
		return nil, http.StatusForbidden, "forbidden", map[string]any{"sandbox_id": id.String()}
	}
	return sb, 0, "", nil
}

func (s *Service) toolSandboxCreate(ctx context.Context, p *auth.Principal, args map[string]any) (any, int, string, map[string]any) {
	if p == nil {
		return nil, http.StatusUnauthorized, "unauthorized", nil
	}
	bm := strings.ToLower(strings.TrimSpace(argString(args, "billing_method")))
	if bm != "lightning" && bm != "credits" {
		return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "billing_method must be lightning or credits"}
	}
	tplName := argString(args, "template")
	if tplName == "" {
		return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "template required"}
	}
	tpl, err := s.Store.GetTemplateByName(ctx, tplName)
	if err != nil || tpl == nil {
		return nil, http.StatusBadRequest, "bad_template", map[string]any{"message": "unknown template"}
	}
	tier := argString(args, "tier")
	if tier == "" {
		tier = tpl.DefaultTier
	}
	pr, err := s.Store.GetTierPricing(ctx, tier)
	if err != nil || pr == nil {
		return nil, http.StatusBadRequest, "bad_tier", map[string]any{"message": "unknown tier"}
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
			return nil, http.StatusUnauthorized, "unauthorized", map[string]any{"message": "api key required for credits sandboxes"}
		}
		sb.UserID = p.UserID
		sb.APIKeyID = p.APIKeyID
	case "lightning":
		switch p.Kind {
		case auth.KindL402:
			if p.AgentID == nil {
				return nil, http.StatusUnauthorized, "unauthorized", map[string]any{"message": "L402 required for lightning sandboxes"}
			}
			sb.AgentID = p.AgentID
		case auth.KindUserJWT:
			if p.UserID == nil {
				return nil, http.StatusUnauthorized, "unauthorized", nil
			}
			ag, err := s.Store.GetAgentByJWTSub(ctx, *p.UserID)
			if err != nil {
				return nil, http.StatusInternalServerError, "store_error", map[string]any{"message": err.Error()}
			}
			if ag == nil {
				return nil, http.StatusUnauthorized, "unauthorized", map[string]any{"message": "register wallet offer first"}
			}
			sb.AgentID = &ag.ID
		default:
			return nil, http.StatusUnauthorized, "unauthorized", map[string]any{"message": "L402 or user JWT with agent required for lightning sandboxes"}
		}
	}

	if err := s.Store.InsertSandbox(ctx, sb); err != nil {
		return nil, http.StatusInternalServerError, "store_error", map[string]any{"message": err.Error()}
	}

	cp := strings.TrimRight(strings.TrimSpace(s.Cfg.ControlPlanePublicURL), "/")
	if cp == "" {
		cp = "http://127.0.0.1:" + s.Cfg.Port
	}

	var initialBal int64
	var rate int64
	var topThresh, topAmt int64
	switch bm {
	case "lightning":
		ag, _ := s.Store.GetAgentByID(ctx, *sb.AgentID)
		if ag != nil {
			initialBal = ag.BalanceMsats
		}
		rate = pr.MsatsPerSecond
		topThresh = int64(s.Cfg.BalanceLowWaterSecs) * rate
		topAmt = int64(s.Cfg.BalanceTopupChunkSecs) * rate
	case "credits":
		alloc, _, err := s.Store.AllocateCreditsForSandbox(ctx, *sb.UserID, sb.ID, s.Cfg.LowCreditThresholdCents*10)
		if err != nil {
			return nil, http.StatusPaymentRequired, "insufficient_credits", map[string]any{"message": err.Error()}
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
		topThresh = int64(s.Cfg.BalanceLowWaterSecs) * rate
		topAmt = int64(s.Cfg.BalanceTopupChunkSecs) * rate
	}

	env := map[string]string{
		"SANDBOX_ID":             sb.ID.String(),
		"POD_ID":                 "",
		"CONTROL_PLANE_URL":      cp,
		"JWT_SECRET":             string(s.Cfg.WSSJWTSecret),
		"CALLBACK_SECRET":        s.Cfg.CallbackSecret,
		"BILLING_METHOD":         bm,
		"RATE_PER_SECOND":        fmt.Sprintf("%d", rate),
		"INITIAL_BALANCE":        fmt.Sprintf("%d", initialBal),
		"TOPUP_THRESHOLD":        fmt.Sprintf("%d", topThresh),
		"TOPUP_AMOUNT":           fmt.Sprintf("%d", topAmt),
		"BALANCE_LOW_WATER_SECS": fmt.Sprintf("%d", s.Cfg.BalanceLowWaterSecs),
	}

	if s.Orch == nil {
		return nil, http.StatusServiceUnavailable, "orchestrator_disabled", map[string]any{"message": "runpod not configured"}
	}
	if err := s.Orch.CreateSandbox(ctx, sb, tpl, env); err != nil {
		return nil, http.StatusBadGateway, "runpod_error", map[string]any{"message": err.Error()}
	}
	sb2, _ := s.Store.GetSandbox(ctx, sb.ID)
	return sb2, 0, "", nil
}

func (s *Service) toolSandboxExec(ctx context.Context, p *auth.Principal, args map[string]any) (any, int, string, map[string]any) {
	id, ok := parseUUIDArg(args, "sandbox_id")
	if !ok {
		return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "sandbox_id"}
	}
	sb, st, slug, ex := s.getSandboxForPrincipal(ctx, p, id)
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
	sb, st, slug, ex := s.getSandboxForPrincipal(ctx, p, id)
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
	sb, st, slug, ex := s.getSandboxForPrincipal(ctx, p, id)
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
	sb, st, slug, ex := s.getSandboxForPrincipal(ctx, p, id)
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
	sb, st, slug, ex := s.getSandboxForPrincipal(ctx, p, id)
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
	sb, st, slug, ex := s.getSandboxForPrincipal(ctx, p, id)
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
	sb, st, slug, ex := s.getSandboxForPrincipal(ctx, p, id)
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
	sb, st, slug, ex := s.getSandboxForPrincipal(ctx, p, id)
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

func (s *Service) toolSandboxExtendBalance(ctx context.Context, p *auth.Principal, args map[string]any) (any, int, string, map[string]any) {
	id, ok := parseUUIDArg(args, "sandbox_id")
	if !ok {
		return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "sandbox_id"}
	}
	sb, st, slug, ex := s.getSandboxForPrincipal(ctx, p, id)
	if sb == nil {
		return nil, st, slug, ex
	}
	switch sb.BillingMethod {
	case "credits":
		if p == nil || p.UserID == nil {
			return nil, http.StatusUnauthorized, "unauthorized", nil
		}
		want := int64(argFloat(args, "cents"))
		if want <= 0 {
			want = s.Cfg.LowCreditThresholdCents * 10
		}
		a, after, err := s.Store.AllocateCreditsForSandbox(ctx, *p.UserID, id, want)
		if err != nil {
			return nil, http.StatusPaymentRequired, "insufficient_credits", map[string]any{"message": err.Error()}
		}
		return map[string]any{"allocated_cents": a, "balance_after_cents": after}, 0, "", nil
	case "lightning":
		agentID := uuid.Nil
		switch {
		case p != nil && p.AgentID != nil:
			agentID = *p.AgentID
		case p != nil && p.Kind == auth.KindUserJWT && p.UserID != nil:
			ag, err := s.Store.GetAgentByJWTSub(ctx, *p.UserID)
			if err != nil || ag == nil {
				return nil, http.StatusUnauthorized, "unauthorized", map[string]any{"message": "agent not found for user"}
			}
			agentID = ag.ID
		default:
			return nil, http.StatusUnauthorized, "unauthorized", map[string]any{"message": "agent context required"}
		}
		if s.PaySvc == nil {
			return nil, http.StatusServiceUnavailable, "payment_disabled", map[string]any{"message": "lnd not configured"}
		}
		sats := int64(argFloat(args, "sats"))
		if sats <= 0 {
			sats = 50_000
		}
		idem := argString(args, "idempotency_key")
		if idem == "" {
			idem = fmt.Sprintf("mcp-extend:%s:%d", id.String(), time.Now().UnixNano())
		}
		memo := fmt.Sprintf("mcp-extend:sandbox:%s", id.String())
		dep, err := s.PaySvc.RequestDepositInvoice(ctx, agentID, sats, 3600, memo, idem)
		if err != nil {
			return nil, http.StatusBadGateway, slugFromPaymentClass(string(payment.Classify(err))), map[string]any{"message": err.Error()}
		}
		if dep == nil || len(dep.RHash) != 32 {
			return nil, http.StatusBadGateway, "invoice_failed", map[string]any{"message": "bad invoice"}
		}
		expRFC := time.Now().UTC().Add(time.Duration(dep.ExpirySeconds) * time.Second).Format(time.RFC3339)
		ag3, _ := s.Store.GetAgentByID(ctx, agentID)
		wlvl := ""
		if ag3 != nil {
			wlvl = ag3.WarningLevel
		}
		return map[string]any{
			"bolt11":         dep.PaymentRequest,
			"payment_hash":   hex.EncodeToString(dep.RHash),
			"amount_msats":   dep.AmountMsats,
			"expires_at":     expRFC,
			"warning_level": wlvl,
		}, 0, "", nil
	default:
		return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "billing method"}
	}
}

func (s *Service) toolWalletBalance(ctx context.Context, p *auth.Principal) (any, int, string, map[string]any) {
	if p == nil {
		return nil, http.StatusUnauthorized, "unauthorized", nil
	}
	var ag *store.Agent
	var err error
	switch p.Kind {
	case auth.KindL402:
		if p.AgentID == nil {
			return nil, http.StatusUnauthorized, "unauthorized", nil
		}
		ag, err = s.Store.GetAgentByID(ctx, *p.AgentID)
	case auth.KindUserJWT, auth.KindAPIKey:
		if p.UserID == nil {
			return nil, http.StatusUnauthorized, "unauthorized", nil
		}
		ag, err = s.Store.GetAgentByJWTSub(ctx, *p.UserID)
	default:
		return nil, http.StatusUnauthorized, "unauthorized", map[string]any{"message": "unsupported principal kind"}
	}
	if err != nil {
		return nil, http.StatusInternalServerError, "store_error", map[string]any{"message": err.Error()}
	}
	if ag == nil {
		return nil, http.StatusNotFound, "not_found", map[string]any{"message": "register wallet first"}
	}
	rate, _ := s.Store.MaxMsatsPerSecondRunningSandboxesForAgent(ctx, ag.ID)
	if rate <= 0 {
		pr, err2 := s.Store.GetTierPricing(ctx, "nano")
		if err2 != nil || pr == nil {
			return nil, http.StatusInternalServerError, "store_error", map[string]any{"message": "tier pricing"}
		}
		rate = pr.MsatsPerSecond
	}
	nextAction, minD, sug := lightningwallet.Hints(ag.BalanceMsats, rate, s.Cfg)
	active, _ := s.Store.CountRunningSandboxesByAgent(ctx, ag.ID)
	cp := strings.TrimRight(strings.TrimSpace(s.Cfg.ControlPlanePublicURL), "/")
	depositURL := ""
	if cp != "" {
		depositURL = cp + "/v1/wallet/deposit"
	}
	return map[string]any{
		"balance_msats":             ag.BalanceMsats,
		"active_sandboxes":         active,
		"billing_hint_msats_sec":    rate,
		"warning_level":             ag.WarningLevel,
		"next_action":               nextAction,
		"min_deposit_msats":        minD,
		"suggested_deposit_msats":  sug,
		"deposit_url":               depositURL,
	}, 0, "", nil
}

func resolveAgentUUID(ctx context.Context, s *Service, p *auth.Principal) (uuid.UUID, error) {
	if p == nil {
		return uuid.Nil, fmt.Errorf("no principal")
	}
	switch {
	case p.AgentID != nil:
		return *p.AgentID, nil
	case p.Kind == auth.KindUserJWT && p.UserID != nil:
		ag, err := s.Store.GetAgentByJWTSub(ctx, *p.UserID)
		if err != nil || ag == nil {
			return uuid.Nil, fmt.Errorf("agent not registered")
		}
		return ag.ID, nil
	default:
		return uuid.Nil, fmt.Errorf("agent context required")
	}
}

func (s *Service) toolWalletDeposit(ctx context.Context, p *auth.Principal, args map[string]any) (any, int, string, map[string]any) {
	if s.PaySvc == nil || s.PaySvc.LND == nil {
		return nil, http.StatusServiceUnavailable, "payment_disabled", map[string]any{"message": "lnd not configured"}
	}
	agentID, err := resolveAgentUUID(ctx, s, p)
	if err != nil {
		return nil, http.StatusUnauthorized, "unauthorized", map[string]any{"message": err.Error()}
	}
	sats := int64(argFloat(args, "amount_sats"))
	idem := argString(args, "idempotency_key")
	expiry := int64(3600)
	memo := fmt.Sprintf("mcp-wallet-deposit:agent:%s", agentID)
	dep, err := s.PaySvc.RequestDepositInvoice(ctx, agentID, sats, expiry, memo, strings.TrimSpace(idem))
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
	expRFC := time.Now().UTC().Add(time.Duration(dep.ExpirySeconds) * time.Second).Format(time.RFC3339)
	return map[string]any{
		"bolt11":         dep.PaymentRequest,
		"payment_hash":   hex.EncodeToString(dep.RHash),
		"amount_msats":   dep.AmountMsats,
		"expires_at":     expRFC,
		"min_amount_msats": int64(1000),
	}, 0, "", nil
}

func (s *Service) toolWalletDepositStatus(ctx context.Context, p *auth.Principal, args map[string]any) (any, int, string, map[string]any) {
	agentID, err := resolveAgentUUID(ctx, s, p)
	if err != nil {
		return nil, http.StatusUnauthorized, "unauthorized", map[string]any{"message": err.Error()}
	}
	hhex := strings.TrimSpace(argString(args, "payment_hash"))
	ph, err := hex.DecodeString(hhex)
	if err != nil || len(ph) != 32 {
		return nil, http.StatusBadRequest, "invalid_params", map[string]any{"message": "payment_hash"}
	}
	st, err := s.Store.GetDepositStatusForAgent(ctx, agentID, ph)
	if err != nil {
		return nil, http.StatusInternalServerError, "store_error", map[string]any{"message": err.Error()}
	}
	if st == nil {
		return nil, http.StatusNotFound, "not_found", map[string]any{"message": "unknown payment_hash"}
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
	return out, 0, "", nil
}
