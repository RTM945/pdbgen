package ptable

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var UserRedEnvelope userRedEnvelope

type userRedEnvelope struct{}

type RedEnvelope struct {
	id            int64
	uid           int64
	actId         int32
	lastRefreshAt time.Time
	todayCount    int32
	total         int32

	loaded            bool
	origLastRefreshAt time.Time // Load时/上次Update成功后的快照，业务代码不要直接读写，见ResetToLoaded
	origTodayCount    int32     // Load时/上次Update成功后的快照，业务代码不要直接读写，见ResetToLoaded
	origTotal         int32     // Load时/上次Update成功后的快照，业务代码不要直接读写，见ResetToLoaded
	dirty             map[string]struct{}
}

func NewRedEnvelope() *RedEnvelope {
	return &RedEnvelope{dirty: make(map[string]struct{})}
}

func loadedRedEnvelope(id int64, uid int64, actId int32, lastRefreshAt time.Time, todayCNT int32, total int32) *RedEnvelope {
	return &RedEnvelope{
		id:                id,
		uid:               uid,
		actId:             actId,
		lastRefreshAt:     lastRefreshAt,
		todayCount:        todayCNT,
		total:             total,
		loaded:            true,
		origLastRefreshAt: lastRefreshAt,
		origTodayCount:    todayCNT,
		origTotal:         total,
		dirty:             make(map[string]struct{}),
	}
}

func (o *RedEnvelope) SetUid(v int64) {
	if o.uid == v {
		return
	}
	o.uid = v
	o.dirty["uid"] = struct{}{}
}

func (o *RedEnvelope) SetActId(v int32) {
	if o.actId == v {
		return
	}
	o.actId = v
	o.dirty["act_id"] = struct{}{}
}

func (o *RedEnvelope) SetLastRefreshAt(v time.Time) {
	if o.origLastRefreshAt == v {
		return
	}
	o.lastRefreshAt = v
	o.dirty["last_refresh_at"] = struct{}{}
}

func (o *RedEnvelope) SetTodayCount(v int32) {
	if o.todayCount == v {
		return
	}
	o.todayCount = v
	o.dirty["today_count"] = struct{}{}
}

func (o *RedEnvelope) SetTotal(v int32) {
	if o.total == v {
		return
	}
	o.total = v
	o.dirty["total"] = struct{}{}
}

func (o *RedEnvelope) Uid() int64 {
	return o.uid
}

func (o *RedEnvelope) ActId() int32 {
	return o.actId
}

func (o *RedEnvelope) LastRefreshAt() time.Time {
	return o.lastRefreshAt
}

func (o *RedEnvelope) TodayCount() int32 {
	return o.todayCount
}

func (o *RedEnvelope) Total() int32 {
	return o.total
}

// ResetToLoaded 把对象的字段还原回Load成功时（或者上一次Update成功时）的快照值，
// 并清空dirty标记——也就是"撤销所有还没真正写进数据库的内存修改"。
//
// 用途：Update失败、或者事务因为别的原因回滚了之后，内存里这个对象可能已经被
// SetXxx改得和数据库实际内容不一样了。如果以后引入本地缓存、把这个对象继续放
// 回缓存供下次读取复用，绝不能让缓存里存着"改了一半、DB其实没写进去"的脏状态——
// 出错时调用这个方法，就能把对象安全地恢复成"确定和DB一致"的版本，再决定是丢弃
// 还是放回缓存都不会有问题。当前实现里没有本地缓存，这个方法先备着。
func (o *RedEnvelope) ResetToLoaded() {
	o.lastRefreshAt = o.origLastRefreshAt
	o.todayCount = o.origTodayCount
	o.total = o.origTotal
	o.dirty = make(map[string]struct{})
}

var ErrUserRedEnvelopeFound = errors.New("user_red_envelope: row not found at update time")

const selectColumnsRedEnvelope = "id, uid, act_id, last_refresh_at, today_count, total"

func scanRowRedEnvelope(row pgx.Row) *RedEnvelope {
	var (
		id            int64
		uid           int64
		actId         int32
		lastRefreshAt time.Time
		todayCNT      int32
		total         int32
	)
	if err := row.Scan(
		&id,
		&uid,
		&actId,
		&lastRefreshAt,
		&todayCNT,
		&total,
	); err != nil {
		panic(err)
	}
	return loadedRedEnvelope(
		id, uid, actId, lastRefreshAt, todayCNT, total,
	)
}

func (userRedEnvelope) GetById(ctx context.Context, id int64) *RedEnvelope {
	tx := txFromCtx(ctx)
	const q = "SELECT " + selectColumnsRedEnvelope + " FROM user_red_envelope WHERE id = $1"
	row := tx.QueryRow(ctx, q, id)
	return scanRowRedEnvelope(row)
}

func (userRedEnvelope) GetByUidActId(ctx context.Context, uid int64, actId int32) *RedEnvelope {
	tx := txFromCtx(ctx)
	const q = "SELECT " + selectColumnsRedEnvelope + " FROM user_red_envelope WHERE uid = $1 and act_id = $2"
	row := tx.QueryRow(ctx, q, uid, actId)
	return scanRowRedEnvelope(row)
}

func (userRedEnvelope) Update(ctx context.Context, o *RedEnvelope) error {
	tx := txFromCtx(ctx)
	if !o.loaded {
		return errors.New(": update RedEnvelope must load first")
	}
	if len(o.dirty) == 0 {
		return nil
	}
	var sets []string
	var args []any
	n := 0
	next := func() int { n++; return n }

	if _, ok := o.dirty["last_refresh_at"]; ok {
		sets = append(sets, fmt.Sprintf("last_refresh_at = $%d", next()))
		args = append(args, o.lastRefreshAt)
	}

	if _, ok := o.dirty["today_count"]; ok {
		sets = append(sets, fmt.Sprintf("today_count = $%d", next()))
		args = append(args, o.todayCount)
	}

	if _, ok := o.dirty["total"]; ok {
		sets = append(sets, fmt.Sprintf("total = $%d", next()))
		args = append(args, o.total)
	}

	args = append(args, o.id)
	q := fmt.Sprintf("UPDATE user_red_envelope SET %s WHERE id = $%d",
		strings.Join(sets, ", "), n+1)

	tag, err := tx.Exec(ctx, q, args...)
	if err != nil {
		return err
	}
	// pgconn.CommandTag.RowsAffected() 不返回error——这点和database/sql的
	// sql.Result.RowsAffected()（返回(int64, error)）不一样，pgx这边执行都成功了
	// 才会走到这里，行数本身不会再失败。
	if tag.RowsAffected() == 0 {
		return ErrUserRedEnvelopeFound
	}

	o.origLastRefreshAt = o.lastRefreshAt
	o.origTodayCount = o.todayCount
	o.origTotal = o.total
	o.dirty = make(map[string]struct{})
	return nil
}

func (userRedEnvelope) Insert(ctx context.Context, o *RedEnvelope) {
	tx := txFromCtx(ctx)
	const q = "INSERT INTO user_red_envelope (uid, act_id, last_refresh_at, today_count, total) VALUES ($1, $2, $3, $4, $5) RETURNING id"
	row := tx.QueryRow(ctx, q, o.uid, o.actId, o.lastRefreshAt, o.todayCount, o.total)
	if err := row.Scan(&o.id); err != nil {
		panic(err)
	}
	o.loaded = true
	o.origLastRefreshAt = o.lastRefreshAt
	o.origTodayCount = o.todayCount
	o.origTotal = o.total
	o.dirty = make(map[string]struct{})
}
