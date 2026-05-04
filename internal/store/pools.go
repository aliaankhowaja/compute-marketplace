package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aliaankhowaja/sockt-backend/internal/config"
)

// configurePoolerSafeConn disables pgx server-side named prepared statements (stmtcache_*),
// which conflict with transaction-pooling proxies (e.g. Supabase pooler port 6543).
func configurePoolerSafeConn(cc *pgx.ConnConfig) {
	cc.StatementCacheCapacity = 0
	cc.DescriptionCacheCapacity = 0
	cc.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
}

// New connects tx and session pools from config.
func New(ctx context.Context, cfg *config.Control) (*Store, error) {
	txCfg, err := pgxpool.ParseConfig(cfg.SupabaseDBTxDSN)
	if err != nil {
		return nil, fmt.Errorf("parse tx dsn: %w", err)
	}
	configurePoolerSafeConn(txCfg.ConnConfig)
	txCfg.MaxConns = cfg.DBMaxConns
	txCfg.MinConns = cfg.DBMinConns
	txCfg.MaxConnLifetime = cfg.DBMaxConnLifetime
	txCfg.MaxConnIdleTime = cfg.DBMaxConnIdleTime

	txPool, err := pgxpool.NewWithConfig(ctx, txCfg)
	if err != nil {
		return nil, fmt.Errorf("tx pool: %w", err)
	}

	sessDSN := cfg.SupabaseDBSessionDSN
	if sessDSN == "" {
		sessDSN = cfg.SupabaseDBTxDSN
	}
	sessCfg, err := pgxpool.ParseConfig(sessDSN)
	if err != nil {
		txPool.Close()
		return nil, fmt.Errorf("parse session dsn: %w", err)
	}
	configurePoolerSafeConn(sessCfg.ConnConfig)
	sessCfg.MaxConns = cfg.DBMaxConns
	sessCfg.MinConns = cfg.DBMinConns
	sessCfg.MaxConnLifetime = cfg.DBMaxConnLifetime
	sessCfg.MaxConnIdleTime = cfg.DBMaxConnIdleTime

	sessPool, err := pgxpool.NewWithConfig(ctx, sessCfg)
	if err != nil {
		txPool.Close()
		return nil, fmt.Errorf("session pool: %w", err)
	}

	return &Store{Tx: txPool, Session: sessPool}, nil
}
