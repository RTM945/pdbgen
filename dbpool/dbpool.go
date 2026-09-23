package dbpool

import (
	"context"
	"errors"
	"fmt"
	"pdbgen/readxml"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	dbpool *pgxpool.Pool

	statementTimeoutMs                int
	idleInTransactionSessionTimeoutMs int
	lockTimeoutMs                     int
)

var ErrAdvisoryLockNotAcquired = errors.New("advisory lock not acquired")

func Init(ctx context.Context, pdb *readxml.Schema) error {
	cfg, err := pgxpool.ParseConfig(pdb.URL)
	if err != nil {
		return err
	}
	cfg.MaxConns = pdb.PoolMaxConns
	cfg.MinConns = pdb.PoolMinConns
	cfg.MaxConnLifetime = time.Duration(pdb.PoolMaxConnLifetime) * time.Second
	cfg.MaxConnIdleTime = time.Duration(pdb.PoolMaxConnIdleTime) * time.Second
	cfg.HealthCheckPeriod = time.Duration(pdb.PoolHealthCheckPeriod) * time.Second
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	if pdb.AppName != "" {
		cfg.ConnConfig.RuntimeParams["application_name"] = pdb.AppName
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return err
	}
	dbpool = pool
	statementTimeoutMs = pdb.StatementTimeoutMs
	idleInTransactionSessionTimeoutMs = pdb.IdleInTransactionSessionTimeoutMs
	lockTimeoutMs = pdb.LockTimeoutMs
	return nil
}

func setLocalTimeout(ctx context.Context, tx pgx.Tx) error {
	if statementTimeoutMs > 0 {
		if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = "+strconv.Itoa(statementTimeoutMs)); err != nil {
			return fmt.Errorf("set statement_timeout: %w", err)
		}
	}

	if idleInTransactionSessionTimeoutMs > 0 {
		if _, err := tx.Exec(ctx, "SET LOCAL idle_in_transaction_session_timeout = "+strconv.Itoa(idleInTransactionSessionTimeoutMs)); err != nil {
			return fmt.Errorf("set idle_in_transaction_session_timeout: %w", err)
		}
	}

	if lockTimeoutMs > 0 {
		if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = "+strconv.Itoa(lockTimeoutMs)); err != nil {
			return fmt.Errorf("set lock_timeout: %w", err)
		}
	}

	return nil
}
