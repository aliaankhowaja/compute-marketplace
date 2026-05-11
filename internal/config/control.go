// Package config loads control-plane (sockt-cp) configuration from environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Control is the Cloud Run control plane configuration.
type Control struct {
	Port        string
	ListenHTTP  string
	LogLevel    string
	Environment string

	SupabaseURL            string
	SupabaseServiceRoleKey string
	SupabaseJWTSecret      string
	SupabaseDBTxDSN        string
	SupabaseDBSessionDSN   string

	RunPodAPIKey        string
	RunPodAPIURL        string
	RunPodDefaultRegion string
	// RunPod template IDs for tier-based provisioning (see orchestrator.ResolveTier).
	RunPodTemplateCPUID      string
	RunPodTemplateGPUSmallID string
	RunPodTemplateGPULargeID string

	UsePodMock bool
	PodMockURL string

	// LightningNetwork is mainnet | testnet — used for BOLT11 decode / validation only (not node RPC).
	LightningNetwork string

	LNBOTAPIBaseURL  string // default https://api.ln.bot
	LNBOTAPIKey      string // Bearer uk_... or wk_...
	LNBOTWalletID    string // optional wal_...; empty uses /v1/wallets/current with wk_
	LNBOTHTTPTimeout time.Duration

	LNURLHTTPTimeoutSecs int

	WSSJWTSecret []byte
	DataJWTTTL   time.Duration
	// DataJWTAudience is the JWT aud claim minted onto data_jwt; must match sockt-runtime JWT_AUDIENCE.
	DataJWTAudience string
	// RuntimeSandboxRoot is injected as SANDBOX_ROOT into RunPod pods (path jail + default cwd).
	RuntimeSandboxRoot  string
	CallbackSecret      string
	StripeWebhookSecret string

	BalanceLowWaterSecs     int
	BalanceTopupChunkSecs   int
	LowCreditThresholdCents int64

	APIKeyCacheSize int

	DBMaxConns        int32
	DBMinConns        int32
	DBMaxConnLifetime time.Duration
	DBMaxConnIdleTime time.Duration

	AdminReconcileToken string // optional Bearer for POST /admin/reconcile

	// ControlPlanePublicURL is passed to RunPod pods (e.g. https://api.sockt.dev).
	ControlPlanePublicURL string

	// MCP (Streamable HTTP / JSON-RPC)
	MCPProxyTimeout     time.Duration
	MCPRequestBodyLimit int64
	MCPDataJWTTTL       time.Duration // 0 = use DataJWTTTL for data_jwt minting on MCP→pod proxy
	// MCPRPM is per-principal-or-IP requests per minute for POST /v1/mcp.
	MCPRPM int

	// SandboxPrepaySecs is how many seconds of compute one sandbox-scoped BOLT11 prepays.
	SandboxPrepaySecs int
	// SandboxTopupTriggerSecs is how few seconds of remaining balance triggers a top-up mint.
	SandboxTopupTriggerSecs int

	// SandboxReadyTimeout is the max time to wait for sockt-runtime /healthz after pod create or resume (SANDBOX_READY_TIMEOUT_SECS).
	SandboxReadyTimeout time.Duration
	// SandboxReadyPollInterval is the delay between readiness probes (SANDBOX_READY_POLL_INTERVAL_MS).
	SandboxReadyPollInterval time.Duration
	// SandboxReadyProbeTimeout is the per-request HTTP timeout for each GET /healthz (SANDBOX_READY_PROBE_TIMEOUT_SECS).
	SandboxReadyProbeTimeout time.Duration
}

func parseBool(key string, def bool) (bool, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("%s: invalid boolean", key)
	}
}

func envOrDefault(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	return v
}

func parseInt(key string, def int) (int, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func parseInt64(key string, def int64) (int64, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func parseDurationSecs(key string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return time.Duration(n) * time.Second, nil
}

func parseDurationMillis(key string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s: must be >= 0", key)
	}
	return time.Duration(n) * time.Millisecond, nil
}

// Load reads control-plane config from the environment.
func Load() (*Control, error) {
	c := &Control{
		Port:                     envOrDefault("PORT", "8080"),
		ListenHTTP:               envOrDefault("LISTEN_HTTP", ""),
		LogLevel:                 envOrDefault("LOG_LEVEL", "info"),
		Environment:              envOrDefault("ENV", "dev"),
		SupabaseURL:              strings.TrimSpace(os.Getenv("SUPABASE_URL")),
		SupabaseServiceRoleKey:   strings.TrimSpace(os.Getenv("SUPABASE_SERVICE_ROLE_KEY")),
		SupabaseJWTSecret:        strings.TrimSpace(os.Getenv("SUPABASE_JWT_SECRET")),
		SupabaseDBTxDSN:          strings.TrimSpace(os.Getenv("SUPABASE_DB_TX_DSN")),
		SupabaseDBSessionDSN:     strings.TrimSpace(os.Getenv("SUPABASE_DB_SESSION_DSN")),
		RunPodAPIKey:             strings.TrimSpace(os.Getenv("RUNPOD_API_KEY")),
		RunPodAPIURL:             envOrDefault("RUNPOD_API_URL", "https://rest.runpod.io/v1"),
		RunPodDefaultRegion:      envOrDefault("RUNPOD_DEFAULT_REGION", "US-CA-1"),
		RunPodTemplateCPUID:      strings.TrimSpace(os.Getenv("RUNPOD_TEMPLATE_CPU_ID")),
		RunPodTemplateGPUSmallID: strings.TrimSpace(os.Getenv("RUNPOD_TEMPLATE_GPU_SMALL_ID")),
		RunPodTemplateGPULargeID: strings.TrimSpace(os.Getenv("RUNPOD_TEMPLATE_GPU_LARGE_ID")),
		PodMockURL:               strings.TrimRight(envOrDefault("POD_MOCK_URL", "http://localhost:9090"), "/"),
		LightningNetwork:         envOrDefault("LIGHTNING_NETWORK", envOrDefault("LND_NETWORK", "mainnet")),
		LNBOTAPIBaseURL:          strings.TrimSpace(os.Getenv("LNBOT_API_BASE_URL")),
		LNBOTAPIKey:              strings.TrimSpace(os.Getenv("LNBOT_API_KEY")),
		LNBOTWalletID:            strings.TrimSpace(os.Getenv("LNBOT_WALLET_ID")),
		CallbackSecret:           strings.TrimSpace(os.Getenv("CALLBACK_SECRET")),
		StripeWebhookSecret:      strings.TrimSpace(os.Getenv("STRIPE_WEBHOOK_SECRET")),
		AdminReconcileToken:      strings.TrimSpace(os.Getenv("ADMIN_RECONCILE_TOKEN")),
		ControlPlanePublicURL:    strings.TrimSpace(os.Getenv("CONTROL_PLANE_URL")),
	}

	var err error
	c.UsePodMock, err = parseBool("USE_POD_MOCK", false)
	if err != nil {
		return nil, err
	}

	if c.LNBOTAPIBaseURL == "" {
		c.LNBOTAPIBaseURL = "https://api.ln.bot"
	}
	c.LNBOTHTTPTimeout, err = parseDurationSecs("LNBOT_HTTP_TIMEOUT_SECS", 45*time.Second)
	if err != nil {
		return nil, err
	}

	c.LNURLHTTPTimeoutSecs, err = parseInt("LNURL_HTTP_TIMEOUT_SECS", 8)
	if err != nil {
		return nil, err
	}
	c.BalanceLowWaterSecs, err = parseInt("BALANCE_LOW_WATER_SECS", 60)
	if err != nil {
		return nil, err
	}
	c.BalanceTopupChunkSecs, err = parseInt("BALANCE_TOPUP_CHUNK_SECS", 300)
	if err != nil {
		return nil, err
	}
	c.LowCreditThresholdCents, err = parseInt64("LOW_CREDIT_THRESHOLD_CENTS", 100)
	if err != nil {
		return nil, err
	}
	c.APIKeyCacheSize, err = parseInt("API_KEY_CACHE_SIZE", 10000)
	if err != nil {
		return nil, err
	}

	c.DataJWTTTL, err = parseDurationSecs("DATA_JWT_TTL_SECS", time.Hour)
	if err != nil {
		return nil, err
	}

	c.DataJWTAudience = strings.TrimSpace(os.Getenv("JWT_AUDIENCE"))
	if c.DataJWTAudience == "" {
		c.DataJWTAudience = "sockt-runtime" // keep in sync with auth.DataJWTAudience (runtime data_jwt)
	}
	c.RuntimeSandboxRoot = strings.TrimSpace(os.Getenv("RUNTIME_SANDBOX_ROOT"))
	if c.RuntimeSandboxRoot == "" {
		c.RuntimeSandboxRoot = "/home/sandbox"
	}

	c.MCPProxyTimeout, err = parseDurationSecs("MCP_PROXY_TIMEOUT_SECS", 120*time.Second)
	if err != nil {
		return nil, err
	}
	c.MCPRequestBodyLimit, err = parseInt64("MCP_REQUEST_BODY_LIMIT_BYTES", 4<<20)
	if err != nil {
		return nil, err
	}
	c.MCPDataJWTTTL, err = parseDurationSecs("MCP_DATA_JWT_TTL_SECS", 0)
	if err != nil {
		return nil, err
	}

	c.MCPRPM, err = parseInt("MCP_RPM", 300)
	if err != nil {
		return nil, err
	}
	c.SandboxPrepaySecs, err = parseInt("SANDBOX_PREPAY_SECS", 30)
	if err != nil {
		return nil, err
	}
	if c.SandboxPrepaySecs < 5 {
		c.SandboxPrepaySecs = 5
	}
	c.SandboxTopupTriggerSecs, err = parseInt("SANDBOX_TOPUP_TRIGGER_SECS", 5)
	if err != nil {
		return nil, err
	}
	if c.SandboxTopupTriggerSecs < 1 {
		c.SandboxTopupTriggerSecs = 1
	}

	c.SandboxReadyTimeout, err = parseDurationSecs("SANDBOX_READY_TIMEOUT_SECS", 120*time.Second)
	if err != nil {
		return nil, err
	}
	c.SandboxReadyPollInterval, err = parseDurationMillis("SANDBOX_READY_POLL_INTERVAL_MS", 750*time.Millisecond)
	if err != nil {
		return nil, err
	}
	c.SandboxReadyProbeTimeout, err = parseDurationSecs("SANDBOX_READY_PROBE_TIMEOUT_SECS", 8*time.Second)
	if err != nil {
		return nil, err
	}

	// Pool tuning (MVPplans/06)
	c.DBMaxConns = 15
	c.DBMinConns = 2
	if mc, e := parseInt("DB_MAX_CONNS", 15); e == nil && mc > 0 {
		c.DBMaxConns = int32(mc)
	} else if e != nil {
		return nil, e
	}
	if mc, e := parseInt("DB_MIN_CONNS", 2); e == nil && mc >= 0 {
		c.DBMinConns = int32(mc)
	} else if e != nil {
		return nil, e
	}
	c.DBMaxConnLifetime, err = parseDurationSecs("DB_MAX_CONN_LIFETIME_SECS", 30*time.Minute)
	if err != nil {
		return nil, err
	}
	c.DBMaxConnIdleTime, err = parseDurationSecs("DB_MAX_CONN_IDLE_SECS", 5*time.Minute)
	if err != nil {
		return nil, err
	}

	k := strings.TrimSpace(os.Getenv("WSS_JWT_SECRET"))
	if k == "" {
		return nil, fmt.Errorf("WSS_JWT_SECRET is required")
	}
	c.WSSJWTSecret = []byte(k)

	if c.SupabaseDBTxDSN == "" {
		return nil, fmt.Errorf("SUPABASE_DB_TX_DSN is required")
	}
	if c.CallbackSecret == "" {
		return nil, fmt.Errorf("CALLBACK_SECRET is required")
	}

	return c, nil
}
