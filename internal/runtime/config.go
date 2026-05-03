package runtime

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is loaded from environment variables for the in-pod runtime.
type Config struct {
	SandboxID string
	PodID     string

	ControlPlaneURL string

	JWTSecret []byte

	BillingMethod string // lightning | credits

	RatePerSecond int64
	InitialBalance int64

	TopupThreshold int64
	TopupAmount    int64
	SafetyFloor    int64

	CallbackSecret string

	ListenAddr string

	TLSCertFile string
	TLSKeyFile  string

	SandboxUser string
}

// Load reads configuration from the process environment and validates required fields.
func Load() (*Config, error) {
	c := &Config{
		SandboxID:       strings.TrimSpace(os.Getenv("SANDBOX_ID")),
		PodID:           strings.TrimSpace(os.Getenv("POD_ID")),
		ControlPlaneURL: strings.TrimSpace(os.Getenv("CONTROL_PLANE_URL")),
		JWTSecret:       []byte(strings.TrimSpace(os.Getenv("JWT_SECRET"))),
		BillingMethod:   strings.ToLower(strings.TrimSpace(envOrDefault("BILLING_METHOD", "lightning"))),
		CallbackSecret:  strings.TrimSpace(os.Getenv("CALLBACK_SECRET")),
		ListenAddr:      envOrDefault("LISTEN_ADDR", ":8443"),
		TLSCertFile:     strings.TrimSpace(os.Getenv("TLS_CERT_FILE")),
		TLSKeyFile:      strings.TrimSpace(os.Getenv("TLS_KEY_FILE")),
		SandboxUser:     envOrDefault("SANDBOX_USER", "sandbox"),
	}

	var err error
	c.RatePerSecond, err = parseInt64Env("RATE_PER_SECOND", 10)
	if err != nil {
		return nil, err
	}
	c.InitialBalance, err = parseInt64Env("INITIAL_BALANCE", 100_000)
	if err != nil {
		return nil, err
	}
	c.TopupThreshold, err = parseInt64Env("TOPUP_THRESHOLD", 10_000)
	if err != nil {
		return nil, err
	}
	c.TopupAmount, err = parseInt64Env("TOPUP_AMOUNT", 50_000)
	if err != nil {
		return nil, err
	}
	c.SafetyFloor, err = parseInt64Env("SAFETY_FLOOR", 0)
	if err != nil {
		return nil, err
	}

	switch c.BillingMethod {
	case "lightning", "credits":
	default:
		return nil, fmt.Errorf("BILLING_METHOD must be lightning or credits")
	}

	if c.SandboxID == "" {
		return nil, fmt.Errorf("SANDBOX_ID is required")
	}
	if c.PodID == "" {
		return nil, fmt.Errorf("POD_ID is required")
	}
	if c.ControlPlaneURL == "" {
		return nil, fmt.Errorf("CONTROL_PLANE_URL is required")
	}
	if len(c.JWTSecret) == 0 {
		return nil, fmt.Errorf("JWT_SECRET is required")
	}
	if c.CallbackSecret == "" {
		return nil, fmt.Errorf("CALLBACK_SECRET is required")
	}
	if c.RatePerSecond <= 0 {
		return nil, fmt.Errorf("RATE_PER_SECOND must be positive")
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return nil, fmt.Errorf("TLS_CERT_FILE and TLS_KEY_FILE must both be set or both empty")
	}

	return c, nil
}

func envOrDefault(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	return v
}

func parseInt64Env(key string, defaultVal int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return defaultVal, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", key, err)
	}
	return n, nil
}
