package mcp

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/aliaankhowaja/sockt-backend/internal/auth"
)

const defaultSandboxRoot = "/home/sandbox"

// sandboxInstructions returns MCP initialize preamble text so clients know sandbox layout and path rules.
func sandboxInstructions(svc *Service) string {
	root := defaultSandboxRoot
	if svc != nil && svc.Cfg != nil {
		if r := strings.TrimSpace(svc.Cfg.RuntimeSandboxRoot); r != "" {
			root = filepath.Clean(r)
		}
	}
	exSub := filepath.Join(root, "myproject")
	return fmt.Sprintf(`Sockt sandbox environment:
- The sandbox home directory (SANDBOX_ROOT) and default working directory for sandbox_exec is %q.
- Omit sandbox_exec's working_dir or use "." to run commands from the sandbox home; only set working_dir when you need a subdirectory.
- working_dir is a logical path under SANDBOX_ROOT: "." and "/" refer to the sandbox home; a relative segment like "myproject" maps to %q. An absolute path is accepted only when it equals SANDBOX_ROOT or lies under it (e.g. %q).
- sandbox_write_file, sandbox_read_file, and sandbox_list_files use the same logical path rules for their path argument.
- New sandboxes start with an empty filesystem; use sandbox_list_files to inspect before assuming files exist.

Execution model:
- sandbox_exec returns immediately with execution_id and status "running". Output is NOT in this response.
- Call sandbox_exec_result with the execution_id to retrieve buffered output chunks incrementally.
- Keep polling sandbox_exec_result until status changes from "running" to "completed", "failed", or "cancelled".
- Use sandbox_exec_cancel to abort a long-running command.
- Multiple commands can run concurrently (up to 10 per session).
- Completed executions are available for polling for a limited time; poll promptly.

Billing (lightning sandboxes):
- Balance decreases every second while the sandbox runs.
- sandbox_status returns next_poll_secs: wait exactly that many seconds before your next sandbox_status call. It adapts automatically (5-60 s range: short when balance is nearly depleted, longer when healthy).
- Check warning_level:
  "provisioning" = sandbox is booting after payment; poll every 5 s until status="running".
  "ok" = healthy; poll again in next_poll_secs.
  "low_balance" = running low; a pending_invoice will appear soon. Poll in next_poll_secs.
  "awaiting_payment" = pay the pending_invoice immediately. Poll every next_poll_secs (5 s) until payment confirms.
  "out_of_balance" = sandbox is paused; pay the invoice and call sandbox_resume. Poll every next_poll_secs (5 s).
- pending_invoice appears in both sandbox_status AND sandbox_exec_result responses.
- Pay the BOLT11 invoice immediately when you see it. If unpaid, sandbox auto-pauses.
- POLLING RULE: After every sandbox_status response, wait next_poll_secs before calling sandbox_status again.
- REFUND ON TERMINATE: When you are done with the sandbox, call sandbox_terminate. If you have a lightning address (optional), pass it as lightning_address parameter to receive a refund of any unused prepaid balance. The refund is sent asynchronously.

Billing (credits sandboxes):
- Usage charges your USD credit balance automatically. No invoices to pay. No lightning payments required.
- If credits run out, sandbox pauses. Top up credits externally and call sandbox_resume.
- When terminating: Simply call sandbox_terminate with confirm=true. No lightning_address needed; billing stops immediately.

See result.sockt for Sockt authentication hints.`,
		root, exSub, root)
}

// sessionHints returns structured MCP initialize hints describing auth + onboarding for the caller.
func sessionHints(svc *Service, p *auth.Principal) map[string]any {
	out := map[string]any{}
	if svc != nil && svc.Cfg != nil {
		cp := strings.TrimRight(strings.TrimSpace(svc.Cfg.ControlPlanePublicURL), "/")
		if cp != "" {
			out["control_plane_public_url"] = cp
		}
	}

	if p != nil {
		out["auth_state"] = "authenticated"
		out["principal_kind"] = string(p.Kind)
		switch p.Kind {
		case auth.KindAPIKey:
			out["billing"] = "API key (credits): sandbox_create with billing_method=credits charges your USD credit balance automatically. No lightning payments or invoices. Usage is metered per-second and deducted from your account."
		case auth.KindSandbox:
			out["billing"] = "Sandbox token: bound to a single Lightning sandbox. Poll sandbox_status every next_poll_secs (returned in each response, 5-60 s) and pay any pending_invoice BOLT11 immediately when it appears."
			if p.SandboxID != nil {
				out["sandbox_id"] = p.SandboxID.String()
			}
		default:
			out["billing"] = "See tool descriptions for this credential type."
		}
		return out
	}

	prepay := 30
	if svc != nil && svc.Cfg != nil && svc.Cfg.SandboxPrepaySecs > 0 {
		prepay = svc.Cfg.SandboxPrepaySecs
	}
	out["auth_state"] = "unauthenticated"
	out["billing"] = "Without credentials, sandbox_create with billing_method=lightning and prepaid_sats returns a BOLT11 + sandbox_token. Set prepaid_sats to the number of satoshis you want to prepay. Call sandbox_list_tiers to see pricing for all tiers. Pay the invoice and call subsequent tools with Authorization: Bearer <sandbox_token> (or pass sandbox_token as a tool argument)."
	out["lightning_prepay_secs"] = prepay
	out["onboarding_steps"] = []string{
		`Call sandbox_list_tiers to see available tiers and their pricing (rates in msats_per_second for lightning, usd_cents_per_second for credits, plus cost examples).`,
		`tools/call sandbox_create with billing_method "lightning", tier (nano|micro|standard|gpu_small|gpu_large), and prepaid_sats (amount in satoshis to prepay based on tier pricing).`,
		"Server returns sandbox_id, sandbox_token (sbx_…), and a BOLT11 invoice for the prepaid_sats amount you specified. Pay it from any LN wallet.",
		`After payment is detected (~15 s), status becomes "pending" (warning_level="provisioning") while the sandbox provisions (30-90 s). Poll sandbox_status every 5 s until status="running".`,
		`Poll sandbox_status at the interval given by next_poll_secs in each response (5-60 s, adapts to remaining balance). When warning_level is "low_balance" or "awaiting_payment" with a pending_invoice, pay the BOLT11 immediately.`,
		`If warning_level reaches "out_of_balance", the sandbox auto-pauses; pay the invoice and call sandbox_resume. Keep polling at next_poll_secs until warning_level returns to "ok".`,
	}
	return out
}

type toolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

func toolList(svc *Service, p *auth.Principal) []toolDef {
	_ = svc
	createDesc := "Provision a new sandbox. billing_method: 'lightning' (pay-as-you-go with BOLT11 invoices, no credentials needed) or 'credits' (prepaid USD balance, requires API key). Lightning: returns invoice + sandbox_token immediately, boots 30-90 s after payment is confirmed, requires paying top-up invoices during use. Credits: provisions in 30-90 s, charges automatically, no invoices. Tier: nano|micro|standard|gpu_small|gpu_large. For lightning: pass prepaid_sats to control initial balance. Call sandbox_list_tiers to get pricing for all tiers."
	if p == nil {
		createDesc = "[Without credentials, billing_method defaults to \"lightning\". The response includes a BOLT11 invoice and a sandbox_token; pay the invoice, then send Authorization: Bearer <sandbox_token> on subsequent calls or pass sandbox_token as a tool argument.] " + createDesc
	}
	return []toolDef{
		{
			Name:        "sandbox_list_tiers",
			Description: "List all available sandbox tiers with their pricing information. Returns rates for both lightning (msats_per_second, sats_per_second) and credits (usd_cents_per_second) billing, plus cost examples for common durations (per minute, hour, day). Call this before sandbox_create to understand pricing.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {}
			}`),
		},
		{
			Name:        "sandbox_create",
			Description: createDesc,
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["tier"],
				"properties": {
					"billing_method": {"type": "string", "enum": ["lightning", "credits"]},
					"tier": {"type": "string", "enum": ["nano", "micro", "standard", "gpu_small", "gpu_large"]},
					"label": {"type": "string"},
					"prepaid_sats": {"type": "integer", "minimum": 1, "description": "Lightning only: override default prepay amount (sats). If omitted, server calculates based on tier rate."}
				}
			}`),
		},
		{
			Name:        "sandbox_exec",
			Description: "Run a shell command asynchronously. Returns immediately with execution_id and status \"running\". Output is NOT included in this response. You MUST call sandbox_exec_result repeatedly with the execution_id to retrieve buffered stdout/stderr chunks. Keep polling until status is \"completed\", \"failed\", or \"cancelled\". Pass timeout_ms=0 for unlimited duration (model training, etc). Use sandbox_exec_cancel to abort. working_dir defaults to SANDBOX_ROOT.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["sandbox_id", "command"],
				"properties": {
					"sandbox_id": {"type": "string"},
					"sandbox_token": {"type": "string"},
					"command": {"type": "string"},
					"working_dir": {"type": "string"},
					"timeout_ms": {"type": "integer", "description": "Timeout in milliseconds. Default 30000 (30s), max 300000 (5min). Set 0 for unlimited (long-running tasks)."}
				}
			}`),
		},
		{
			Name:        "sandbox_exec_result",
			Description: "Poll execution output. Returns new stdout/stderr chunks since last poll, current status (running/completed/failed/cancelled), and exit_code when done. Also includes pending_invoice if the sandbox needs payment. Call repeatedly until status is no longer \"running\".",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["execution_id"],
				"properties": {
					"execution_id": {"type": "string"},
					"sandbox_token": {"type": "string"}
				}
			}`),
		},
		{
			Name:        "sandbox_exec_cancel",
			Description: "Cancel a running execution by execution_id. Must be called from the same session that started the execution. Returns immediately with status \"cancelled\".",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["execution_id"],
				"properties": {
					"execution_id": {"type": "string"},
					"sandbox_token": {"type": "string"}
				}
			}`),
		},
		{
			Name:        "sandbox_write_file",
			Description: "Write a file inside the sandbox (base64 on wire to runtime). Pass sandbox_token if not using bearer auth.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["sandbox_id", "path", "content"],
				"properties": {
					"sandbox_id": {"type": "string"},
					"sandbox_token": {"type": "string"},
					"path": {"type": "string"},
					"content": {"type": "string"},
					"encoding": {"type": "string", "enum": ["utf8", "base64"]},
					"create_dirs": {"type": "boolean"}
				}
			}`),
		},
		{
			Name:        "sandbox_read_file",
			Description: "Read a file from the sandbox. Pass sandbox_token if not using bearer auth.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["sandbox_id", "path"],
				"properties": {
					"sandbox_id": {"type": "string"},
					"sandbox_token": {"type": "string"},
					"path": {"type": "string"},
					"encoding": {"type": "string", "enum": ["utf8", "base64"]},
					"max_bytes": {"type": "integer"}
				}
			}`),
		},
		{
			Name:        "sandbox_list_files",
			Description: "List files in a directory inside the sandbox. Pass sandbox_token if not using bearer auth.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["sandbox_id"],
				"properties": {
					"sandbox_id": {"type": "string"},
					"sandbox_token": {"type": "string"},
					"path": {"type": "string"},
					"recursive": {"type": "boolean"},
					"max_depth": {"type": "integer"}
				}
			}`),
		},
		{
			Name:        "sandbox_status",
			Description: "Get sandbox status, billing state, and runtime metrics. For lightning sandboxes: returns prepaid_balance_msats, seconds_remaining, msats_per_second, warning_level, next_poll_secs, and pending_invoice when payment is needed. IMPORTANT: call sandbox_status again after next_poll_secs seconds — it adapts automatically (short when balance is low, longer when healthy, range 5-60 s). Pay the pending_invoice BOLT11 immediately when present.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["sandbox_id"],
				"properties": {
					"sandbox_id": {"type": "string"},
					"sandbox_token": {"type": "string"}
				}
			}`),
		},
		{
			Name:        "sandbox_pause",
			Description: "Pause a running sandbox.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["sandbox_id"],
				"properties": {
					"sandbox_id": {"type": "string"},
					"sandbox_token": {"type": "string"}
				}
			}`),
		},
		{
			Name:        "sandbox_resume",
			Description: "Resume a paused sandbox.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["sandbox_id"],
				"properties": {
					"sandbox_id": {"type": "string"},
					"sandbox_token": {"type": "string"}
				}
			}`),
		},
		{
			Name:        "sandbox_terminate",
			Description: "Permanently delete the sandbox and pod. For lightning sandboxes, pass lightning_address to receive a refund of any remaining prepaid balance.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["sandbox_id", "confirm"],
				"properties": {
					"sandbox_id": {"type": "string"},
					"sandbox_token": {"type": "string"},
					"confirm": {"type": "boolean"},
					"lightning_address": {"type": "string", "description": "Lightning address to refund remaining prepaid balance (lightning billing only)."}
				}
			}`),
		},
	}
}
