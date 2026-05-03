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

	LNDGRPCHost     string
	LNDTLSCertPath  string
	LNDTLSCertPEM   string
	LNDMacaroonPath string
	LNDMacaroonHex  string
	LNDNetwork      string // mainnet | testnet (for offer validation)

	LNDKHTTPURL          string
	LNURLHTTPTimeoutSecs int

	WSSJWTSecret        []byte
	L402RootKey         []byte // 32 bytes; macaroon root
	DataJWTTTL          time.Duration
	CallbackSecret      string
	StripeWebhookSecret string

	BalanceLowWaterSecs     int
	BalanceTopupChunkSecs   int
	LowCreditThresholdCents int64
	InvoicePoolTarget       int
	InvoicePoolRefillBelow  int

	APIKeyCacheSize int

	DBMaxConns        int32
	DBMinConns        int32
	DBMaxConnLifetime time.Duration
	DBMaxConnIdleTime time.Duration

	AdminReconcileToken string // optional Bearer for POST /admin/reconcile

	// ControlPlanePublicURL is passed to RunPod pods (e.g. https://api.sockt.dev).
	ControlPlanePublicURL string
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

// Load reads control-plane config from the environment.
func Load() (*Control, error) {
	c := &Control{
		Port:                   envOrDefault("PORT", "8080"),
		ListenHTTP:             envOrDefault("LISTEN_HTTP", ""),
		LogLevel:               envOrDefault("LOG_LEVEL", "info"),
		Environment:            envOrDefault("ENV", "dev"),
		SupabaseURL:            strings.TrimSpace(os.Getenv("SUPABASE_URL")),
		SupabaseServiceRoleKey: strings.TrimSpace(os.Getenv("SUPABASE_SERVICE_ROLE_KEY")),
		SupabaseJWTSecret:      strings.TrimSpace(os.Getenv("SUPABASE_JWT_SECRET")),
		SupabaseDBTxDSN:        strings.TrimSpace(os.Getenv("SUPABASE_DB_TX_DSN")),
		SupabaseDBSessionDSN:   strings.TrimSpace(os.Getenv("SUPABASE_DB_SESSION_DSN")),
		RunPodAPIKey:           strings.TrimSpace(os.Getenv("RUNPOD_API_KEY")),
		RunPodAPIURL:           envOrDefault("RUNPOD_API_URL", "https://rest.runpod.io/v1"),
		RunPodDefaultRegion:    envOrDefault("RUNPOD_DEFAULT_REGION", "US-CA-1"),
		LNDGRPCHost:            strings.TrimSpace(os.Getenv("LND_GRPC_HOST")),
		LNDTLSCertPath:         strings.TrimSpace(os.Getenv("LND_TLS_CERT_PATH")),
		LNDMacaroonPath:        strings.TrimSpace(os.Getenv("LND_MACAROON_PATH")),
		LNDMacaroonHex:         strings.TrimSpace(os.Getenv("LND_MACAROON_HEX")),
		LNDNetwork:             envOrDefault("LND_NETWORK", "mainnet"),
		LNDKHTTPURL:            strings.TrimSpace(os.Getenv("LNDK_HTTP_URL")),
		CallbackSecret:         strings.TrimSpace(os.Getenv("CALLBACK_SECRET")),
		StripeWebhookSecret:    strings.TrimSpace(os.Getenv("STRIPE_WEBHOOK_SECRET")),
		AdminReconcileToken:    strings.TrimSpace(os.Getenv("ADMIN_RECONCILE_TOKEN")),
		ControlPlanePublicURL:  strings.TrimSpace(os.Getenv("CONTROL_PLANE_URL")),
	}
	pem := strings.TrimSpace(os.Getenv("LND_TLS_CERT_PEM"))
	if pem != "" {
		c.LNDTLSCertPEM = pem
	}

	var err error
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
	c.InvoicePoolTarget, err = parseInt("INVOICE_POOL_TARGET", 100)
	if err != nil {
		return nil, err
	}
	c.InvoicePoolRefillBelow, err = parseInt("INVOICE_POOL_REFILL_BELOW", 40)
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

	lk := strings.TrimSpace(os.Getenv("L402_ROOT_KEY"))
	if lk != "" {
		// hex 64 chars = 32 bytes
		if len(lk) != 64 {
			return nil, fmt.Errorf("L402_ROOT_KEY must be 64 hex chars (32 bytes)")
		}
		var b [32]byte
		for i := 0; i < 32; i++ {
			v, err := strconv.ParseUint(lk[2*i:2*i+2], 16, 8)
			if err != nil {
				return nil, fmt.Errorf("L402_ROOT_KEY hex: %w", err)
			}
			b[i] = byte(v)
		}
		c.L402RootKey = b[:]
	} else {
		// derive from WSS secret via copy (MVP fallback — user should set L402_ROOT_KEY in prod)
		if len(c.WSSJWTSecret) >= 32 {
			c.L402RootKey = append([]byte(nil), c.WSSJWTSecret[:32]...)
		} else {
			pad := make([]byte, 32)
			copy(pad, c.WSSJWTSecret)
			c.L402RootKey = pad
		}
	}

	if c.SupabaseDBTxDSN == "" {
		return nil, fmt.Errorf("SUPABASE_DB_TX_DSN is required")
	}
	if c.CallbackSecret == "" {
		return nil, fmt.Errorf("CALLBACK_SECRET is required")
	}

	return c, nil
}
