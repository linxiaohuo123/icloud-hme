package account

import (
	"errors"
	"testing"
	"time"

	"icloud-hme/internal/hme"
)

// TestPendingLoginSessionLifecycle 验证 2FA 登录挂起会话在内存中的生命周期与驱逐。
func TestPendingLoginSessionLifecycle(t *testing.T) {
	dir := t.TempDir()
	mgr, err := NewManager(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	accID := "acc_test_2fa"
	mgr.mu.Lock()
	mgr.accounts[accID] = &Account{
		ID:          accID,
		Name:        "2FA Test",
		ICloudEmail: "test2fa@icloud.com",
		Host:        "icloud.com",
		Status:      "pending",
	}
	mgr.mu.Unlock()

	// 模拟一个挂起的 client
	mockClient, err := hme.NewClient(nil, "icloud.com", "", false)
	if err != nil {
		t.Fatal(err)
	}

	mgr.mu.Lock()
	mgr.pendingLogins[accID] = &pendingLogin{
		client:    mockClient,
		createdAt: time.Now(),
	}
	mgr.mu.Unlock()

	// 验证挂起状态存在
	mgr.mu.RLock()
	pl, exists := mgr.pendingLogins[accID]
	mgr.mu.RUnlock()
	if !exists || pl == nil {
		t.Fatal("期望存在挂起的 2FA 登录会话")
	}

	// 模拟过期驱逐：将时间调到 10 分钟前
	mgr.mu.Lock()
	pl.createdAt = time.Now().Add(-10 * time.Minute)
	mgr.mu.Unlock()

	// 调用 HMEClientWithPassword 会触发过期清理
	_, loginErr := mgr.HMEClientWithPassword(accID, "dummy-pass", func() (string, error) {
		return "123456", nil
	})
	// 由于网络连接或 dummy-pass 会失败，但关键是挂起的旧会话已被清理
	if loginErr == nil {
		t.Fatal("期望登录失败")
	}

	mgr.mu.RLock()
	_, existsAfter := mgr.pendingLogins[accID]
	mgr.mu.RUnlock()
	// 如果是全新握手且报错（非 ErrOTPRequired），pendingLogins 不应残留
	if existsAfter && !errors.Is(loginErr, hme.ErrOTPRequired) {
		t.Fatal("过期会话应该已被清理且无新挂起")
	}
}

func TestPendingLoginRemovedOnAccountDelete(t *testing.T) {
	dir := t.TempDir()
	mgr, err := NewManager(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	accID := "acc_del_2fa"
	mgr.mu.Lock()
	mgr.accounts[accID] = &Account{
		ID:          accID,
		Name:        "2FA Delete Test",
		ICloudEmail: "del2fa@icloud.com",
		Host:        "icloud.com",
		Status:      "pending",
	}
	mockClient, _ := hme.NewClient(nil, "icloud.com", "", false)
	mgr.pendingLogins[accID] = &pendingLogin{
		client:    mockClient,
		createdAt: time.Now(),
	}
	mgr.mu.Unlock()

	// 删除账号
	if !mgr.RemoveAccount(accID) {
		t.Fatal("删除账号应该成功")
	}

	// 验证 pending 会话已被清理
	mgr.mu.RLock()
	_, exists := mgr.pendingLogins[accID]
	mgr.mu.RUnlock()
	if exists {
		t.Fatal("删除账号后 pending 登录会话应该被同时清理")
	}
}

// TestPendingLoginRetainedOnOTPFailure 验证验证码校验失败时，挂起会话保留供用户重试
func TestPendingLoginRetainedOnOTPFailure(t *testing.T) {
	dir := t.TempDir()
	mgr, err := NewManager(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	accID := "acc_retry_2fa"
	mgr.mu.Lock()
	mgr.accounts[accID] = &Account{
		ID:          accID,
		Name:        "2FA Retry Test",
		ICloudEmail: "retry2fa@icloud.com",
		Host:        "icloud.com",
		Status:      "pending",
	}
	mockClient, _ := hme.NewClient(nil, "icloud.com", "", false)
	mgr.pendingLogins[accID] = &pendingLogin{
		client:    mockClient,
		createdAt: time.Now(),
	}
	mgr.mu.Unlock()

	// 提交错误验证码（由于 mockClient 没有真实的 pendingAuth，SubmitOTP 会报错）
	_, loginErr := mgr.HMEClientWithPassword(accID, "dummy-pass", func() (string, error) {
		return "000000", nil
	})
	if loginErr == nil {
		t.Fatal("期望校验报错")
	}

	// 验证：挂起会话依然被保留，不应被直接删除
	mgr.mu.RLock()
	pl, exists := mgr.pendingLogins[accID]
	mgr.mu.RUnlock()
	if !exists || pl == nil {
		t.Fatal("OTP 校验失败时，挂起会话应该被保留以供用户在同一会话中修正重试")
	}
	if pl.client != mockClient {
		t.Fatal("挂起会话应保持同一 Client 实例")
	}
}
