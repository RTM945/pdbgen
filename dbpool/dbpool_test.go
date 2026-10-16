package dbpool

import (
	"testing"
)

// TestPG 只验证连接池能起来。初始化在 TestMain 里做，见 identity_test.go。
func TestPG(t *testing.T) {
	setupTestDB(t)
}
