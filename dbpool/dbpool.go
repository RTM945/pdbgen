// Package dbpool 把 pdb.xml 里 <pdb> 根节点声明的连接串和连接池参数，
// 转成一个配置好的 *pgxpool.Pool。
//
// 用的是pgx原生连接池（不是database/sql+驱动那条路），换来的是MinConns/
// HealthCheckPeriod这些参数的真实语义：MinConns是真的会被pgxpool主动维持的
// 下限（后台按需补充连接，不是只在"凑巧还没被回收"时才有），HealthCheckPeriod
// 是真的会定期探活、主动清掉不健康的连接，不是等下次用到才发现连接坏了。
//
// 代价是生成的model代码（gen/template.go）跟着换成了pgx的接口风格
// （Exec/QueryRow，没有Context后缀；pgconn.CommandTag代替sql.Result），
// 不再兼容 database/sql 的 *sql.DB / *sql.Tx。
package dbpool

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pdbgen/schema"
)

// Open 按pdb.xml里的url和pool*配置，打开一个配置好的pgxpool连接池
func Open(ctx context.Context, pdb *schema.PDB) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(pdb.URL)
	if err != nil {
		return nil, fmt.Errorf("解析url属性失败: %w", err)
	}

	cfg.MaxConns = int32(pdb.PoolMaxConns)
	cfg.MinConns = int32(pdb.PoolMinConns)
	cfg.MaxConnLifetime = time.Duration(pdb.PoolMaxConnLifetime) * time.Second
	cfg.MaxConnIdleTime = time.Duration(pdb.PoolMaxConnIdleTime) * time.Second
	cfg.HealthCheckPeriod = time.Duration(pdb.PoolHealthCheckPeriod) * time.Second

	if pdb.AppName != "" {
		cfg.ConnConfig.RuntimeParams["application_name"] = pdb.AppName
	}

	// statement_timeout / idle_in_transaction_session_timeout 不走连接串的
	// "options"参数（那是database/sql+lib/pq那条路的做法），这里用pgxpool的
	// AfterConnect钩子，在每条物理连接刚建立、还没进池子之前，显式SET一次——
	// 效果一样：给这条连接设的会话级超时，只要连接不重建就会一直生效，
	// 池子复用这条连接服务任意后续请求时也不需要重新设置。
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if pdb.StatementTimeoutMs > 0 {
			if _, err := conn.Exec(ctx, fmt.Sprintf("SET statement_timeout = %d", pdb.StatementTimeoutMs)); err != nil {
				return fmt.Errorf("设置statement_timeout失败: %w", err)
			}
		}
		if pdb.IdleInTransactionSessionTimeoutMs > 0 {
			if _, err := conn.Exec(ctx, fmt.Sprintf("SET idle_in_transaction_session_timeout = %d", pdb.IdleInTransactionSessionTimeoutMs)); err != nil {
				return fmt.Errorf("设置idle_in_transaction_session_timeout失败: %w", err)
			}
		}
		return nil
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("创建连接池失败: %w", err)
	}
	return pool, nil
}
