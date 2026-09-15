// Package dbpool 把 pdb.xml 里 <pdb> 根节点声明的连接串和连接池参数，
// 转成一个配置好的 *sql.DB。
//
// 重要说明：这里用的是 database/sql 标准接口 + lib/pq 驱动（和已经生成、验证过的
// model代码保持同一套技术栈——生成代码里的 DBTX 接口方法签名对应的是 *sql.Row/
// sql.Result，这是 database/sql 的类型，不是 pgx 原生连接池 pgxpool.Pool 的类型）。
//
// 这个选择带来一个后果：database/sql 的连接池只有4个可调参数
// （SetMaxOpenConns/SetMaxIdleConns/SetConnMaxLifetime/SetConnMaxIdleTime），
// 没有 pgxpool 那种"强制保持N个连接常驻热身"的 MinConns，也没有后台定期探活的
// HealthCheckPeriod——这两个概念在 database/sql 里不存在，不是没配对，是这个技术栈
// 压根没有对应的东西。下面用尽量贴近原意的方式做近似，并在注释里说清楚差多少。
package dbpool

import (
	"database/sql"
	"fmt"
	"net/url"
	"strconv"
	"time"

	_ "github.com/lib/pq"

	"pdbgen/schema"
)

// Open 按pdb.xml里的url和pool*配置，打开一个配置好的连接池
func Open(pdb *schema.PDB) (*sql.DB, error) {
	dsn, err := buildDSN(pdb)
	if err != nil {
		return nil, fmt.Errorf("构造连接串失败: %w", err)
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开连接池失败: %w", err)
	}

	db.SetMaxOpenConns(pdb.PoolMaxConns)

	// MinConns近似：database/sql没有"强制保持N个连接常驻"这个概念，
	// 能做到的最接近的效果是"允许最多N个连接在空闲时不被立即关掉"（SetMaxIdleConns），
	// 但这只是"上限"，不是"下限"——冷启动时、或者流量低谷把连接都关掉之后，
	// 池子不会主动把连接数补回MinConns，只有下一次请求进来才会按需重新建连接。
	// 如果你确实需要"服务启动时就预热出N个连接"这种强保证，
	// 说明你需要的是pgxpool原生连接池，而不是database/sql——那意味着要把
	// DBTX接口和生成代码里的 *sql.Row/sql.Result 换成pgx的对应类型，是更大的改动，
	// 现阶段不建议只为了这一个参数去做这个切换。
	idle := pdb.PoolMinConns
	if idle > pdb.PoolMaxConns {
		idle = pdb.PoolMaxConns
	}
	db.SetMaxIdleConns(idle)

	db.SetConnMaxLifetime(time.Duration(pdb.PoolMaxConnLifetime) * time.Second)
	db.SetConnMaxIdleTime(time.Duration(pdb.PoolMaxConnIdleTime) * time.Second)

	// PoolHealthCheckPeriod: database/sql不做后台主动探活，
	// 它是"用的时候才发现连接坏没坏"（惰性检测，查询失败就把连接扔掉重连）。
	// 在这套"乐观锁+短事务"的架构下，请求本来就很频繁地在用短连接，
	// 惰性检测的探活频率其实不会比主动探活差多少，可以不用纠结这个参数没有被真正使用。

	return db, nil
}

// buildDSN 在pdb.url的基础上，把statement_timeout / idle_in_transaction_session_timeout /
// application_name这几个安全相关的会话参数带上——这几个不是用户原始XML里有的属性，
// 是这次补充建议加的，详见使用说明。
func buildDSN(pdb *schema.PDB) (string, error) {
	u, err := url.Parse(pdb.URL)
	if err != nil {
		return "", fmt.Errorf("url属性不是合法的连接串: %w", err)
	}

	q := u.Query()

	if pdb.AppName != "" {
		q.Set("application_name", pdb.AppName)
	}

	// statement_timeout / idle_in_transaction_session_timeout 通过PG的
	// "options" 启动参数下发，等价于连接建立后执行 SET statement_timeout=...;
	// 这是标准libpq连接参数，跟具体用哪个Go驱动无关。
	var opts string
	if pdb.StatementTimeoutMs > 0 {
		opts += " -c statement_timeout=" + strconv.Itoa(pdb.StatementTimeoutMs)
	}
	if pdb.IdleInTransactionSessionTimeoutMs > 0 {
		opts += " -c idle_in_transaction_session_timeout=" + strconv.Itoa(pdb.IdleInTransactionSessionTimeoutMs)
	}
	if opts != "" {
		q.Set("options", opts)
	}

	u.RawQuery = q.Encode()
	return u.String(), nil
}
