package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aliaankhowaja/sockt-backend/internal/config"
)

func TestPingTxWithDatabaseOptional(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run")
	}
	cfg := &config.Control{
		SupabaseDBTxDSN:      dsn,
		SupabaseDBSessionDSN: dsn,
		WSSJWTSecret:         []byte("01234567890123456789012345678901"),
		CallbackSecret:       "cb",
		DBMaxConns:           5,
		DBMinConns:           1,
		DBMaxConnLifetime:    30 * time.Minute,
		DBMaxConnIdleTime:    5 * time.Minute,
	}
	ctx := context.Background()
	st, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.PingTx(ctx); err != nil {
		t.Fatal(err)
	}
}
