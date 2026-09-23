package dbpool

import (
	"context"
)

type UnitOfWork struct {
	items []uowItem
	seen  map[any]struct{}
}

type uowItem struct {
	obj    any
	update func(context.Context) error
}

func NewUnitOfWork() *UnitOfWork {
	return &UnitOfWork{
		items: make([]uowItem, 0, 16),
		seen:  make(map[any]struct{}),
	}
}

// Register 注册一个需要在事务结束时检查 dirty 并持久化的对象。
// 使用 slice 保证 Flush 顺序稳定；map 只用于去重。
func (u *UnitOfWork) Register(obj any, update func(context.Context) error) {
	if obj == nil {
		return
	}

	if _, exists := u.seen[obj]; exists {
		return
	}

	u.seen[obj] = struct{}{}

	u.items = append(u.items, uowItem{
		obj:    obj,
		update: update,
	})
}

func (u *UnitOfWork) Flush(ctx context.Context) error {
	for _, item := range u.items {
		if err := item.update(ctx); err != nil {
			return err
		}
	}

	return nil
}

func (u *UnitOfWork) Len() int {
	return len(u.items)
}
