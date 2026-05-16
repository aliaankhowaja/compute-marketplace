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
- sandbox_exec returns immediately with execution_id and status "running".
- Poll sandbox_exec_result with the execution_id to retrieve incremental output chunks, status, and exit code.
- Use sandbox_exec_cancel with the execution_id to abort a long-running command.
- Multiple commands can run concurrently (up to 10 per session).
- BILLING: The sandbox consumes prepaid balance every second. Poll sandbox_status to check balance and pending invoices. When a pending_invoice appears, pay it immediately to prevent sandbox pause.

See result.sockt for Sockt authentication and billing hints.`,
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
			out["billing"] = "API key (credits): sandbox_create with billing_method=credits charges your USD credit balance."
		case auth.KindSandbox:
			out["billing"] = "Sandbox token: this credential is bound to a single Lightning sandbox; pay any pending_invoice in sandbox_status to keep it running."
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
	out["billing"] = "Without credentials, sandbox_create with billing_method=lightning returns a BOLT11 + sandbox_token. Pay the invoice and call subsequent tools with Authorization: Bearer <sandbox_token> (or pass sandbox_token as a tool argument)."
	out["lightning_prepay_secs"] = prepay
	out["onboarding_steps"] = []string{
		`tools/call sandbox_create with billing_method "lightning" and tier (nano|micro|standard|gpu_small|gpu_large).`,
		fmt.Sprintf("Server returns sandbox_id, sandbox_token (sbx_…), and a BOLT11 prepaying %d seconds of compute. Pay it from any LN wallet.", prepay),
		`Once paid, the sandbox boots automatically. Poll sandbox_status with sandbox_id+sandbox_token until status="running".`,
		`When sandbox_status shows a pending_invoice, pay it to keep running; otherwise the sandbox auto-pauses when prepaid balance hits zero.`,
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
	createDesc := "Provision a new sandbox (RunPod). May take up to ~2 minutes for credits sandboxes; lightning sandboxes return an invoice + sandbox_token immediately and boot once you pay. billing_method: lightning (no credentials needed) or credits (API key required). Tier: nano|micro|standard|gpu_small|gpu_large."
	if p == nil {
		createDesc = "[Without credentials, billing_method defaults to \"lightning\". The response includes a BOLT11 invoice and a sandbox_token; pay the invoice, then send Authorization: Bearer <sandbox_token> on subsequent calls or pass sandbox_token as a tool argument.] " + createDesc
	}
	return []toolDef{
		{
			Name:        "sandbox_create",
			Description: createDesc,
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["tier"],
				"properties": {
					"billing_method": {"type": "string", "enum": ["lightning", "credits"]},
					"tier": {"type": "string", "enum": ["nano", "micro", "standard", "gpu_small", "gpu_large"]},
					"label": {"type": "string"}
				}
			}`),
		},
		{
			Name:        "sandbox_exec",
			Description: "Run a shell command asynchronously. Returns immediately with execution_id and status \"running\". Poll sandbox_exec_result with the execution_id to retrieve output and completion status. Use sandbox_exec_cancel to abort. working_dir defaults to SANDBOX_ROOT. Pass sandbox_token if not using bearer auth.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["sandbox_id", "command"],
				"properties": {
					"sandbox_id": {"type": "string"},
					"sandbox_token": {"type": "string"},
					"command": {"type": "string"},
					"working_dir": {"type": "string"}
				}
			}`),
		},
		{
			Name:        "sandbox_exec_result",
			Description: "Poll execution output. Returns buffered stdout/stderr chunks since the last poll, status (running/completed/failed/cancelled), and exit_code when done. Call repeatedly until status is no longer \"running\".",
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
			Description: "Cancel a running async execution by its execution_id. Returns immediately with status \"cancelled\".",
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
			Description: "Sandbox row plus optional live runtime status. For lightning sandboxes, includes prepaid_balance_msats, seconds_remaining, and pending_invoice when one is outstanding.",
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
			Description: "Pause a sandbox (stop RunPod).",
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
			Description: "Permanently delete the sandbox and pod.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["sandbox_id", "confirm"],
				"properties": {
					"sandbox_id": {"type": "string"},
					"sandbox_token": {"type": "string"},
					"confirm": {"type": "boolean"}
				}
			}`),
		},
	}
}
