package mcp

import "encoding/json"

type toolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

func toolList() []toolDef {
	return []toolDef{
		{
			Name:        "sandbox_create",
			Description: "Provision a new sandbox (RunPod). billing_method: lightning | credits. Requires L402 (lightning) or API key (credits).",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["billing_method", "template"],
				"properties": {
					"billing_method": {"type": "string", "enum": ["lightning", "credits"]},
					"template": {"type": "string"},
					"tier": {"type": "string"},
					"duration_secs": {"type": "integer"},
					"egress": {"type": "boolean"},
					"label": {"type": "string"},
					"idempotency_key": {"type": "string"}
				}
			}`),
		},
		{
			Name:        "sandbox_exec",
			Description: "Run a shell command in the sandbox via the data plane (proxied with data_jwt).",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["sandbox_id", "command"],
				"properties": {
					"sandbox_id": {"type": "string"},
					"command": {"type": "string"},
					"timeout_ms": {"type": "integer"},
					"env": {"type": "object"},
					"working_dir": {"type": "string"}
				}
			}`),
		},
		{
			Name:        "sandbox_write_file",
			Description: "Write a file inside the sandbox (base64 on wire to runtime).",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["sandbox_id", "path", "content"],
				"properties": {
					"sandbox_id": {"type": "string"},
					"path": {"type": "string"},
					"content": {"type": "string"},
					"encoding": {"type": "string", "enum": ["utf8", "base64"]},
					"create_dirs": {"type": "boolean"}
				}
			}`),
		},
		{
			Name:        "sandbox_read_file",
			Description: "Read a file from the sandbox.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["sandbox_id", "path"],
				"properties": {
					"sandbox_id": {"type": "string"},
					"path": {"type": "string"},
					"encoding": {"type": "string", "enum": ["utf8", "base64"]},
					"max_bytes": {"type": "integer"}
				}
			}`),
		},
		{
			Name:        "sandbox_list_files",
			Description: "List files in a directory inside the sandbox.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["sandbox_id"],
				"properties": {
					"sandbox_id": {"type": "string"},
					"path": {"type": "string"},
					"recursive": {"type": "boolean"},
					"max_depth": {"type": "integer"}
				}
			}`),
		},
		{
			Name:        "sandbox_status",
			Description: "Sandbox row from control plane plus optional live runtime status.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["sandbox_id"],
				"properties": {
					"sandbox_id": {"type": "string"}
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
					"sandbox_id": {"type": "string"}
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
					"sandbox_id": {"type": "string"}
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
					"confirm": {"type": "boolean"}
				}
			}`),
		},
		{
			Name:        "sandbox_extend_balance",
			Description: "Top up agent balance (lightning) or allocate credits to sandbox (credits). Use 'sats' for lightning; use 'cents' for credits (optional).",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"required": ["sandbox_id"],
				"properties": {
					"sandbox_id": {"type": "string"},
					"sats": {"type": "integer"},
					"cents": {"type": "integer"},
					"idempotency_key": {"type": "string"}
				}
			}`),
		},
		{
			Name:        "wallet_balance",
			Description: "Lightning agent balance / offer metadata for the authenticated principal.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {}
			}`),
		},
	}
}
