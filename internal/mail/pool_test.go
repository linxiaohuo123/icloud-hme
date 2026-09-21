package mail

import (
	"sync"
	"testing"
)

func TestPoolGetOrCreateAndClose(t *testing.T) {
	p := NewPool()
	defer p.Close()

	// 1. 验证空凭据报错
	err := p.Do("", "", "", func(c *Client) error {
		return nil
	})
	if err == nil {
		t.Fatalf("空凭据应当报错")
	}

	// 2. 验证并发获取同一账号的连接对象安全且归一
	var wg sync.WaitGroup
	var pcs [10]*pooledConn
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			pcs[idx] = p.getOrCreate("User@iCloud.com")
		}(i)
	}
	wg.Wait()

	target := pcs[0]
	for i := 1; i < 10; i++ {
		if pcs[i] != target {
			t.Fatalf("相同账号获取的 pooledConn 指针不一致")
		}
	}

	// 3. 验证 Close 后清空
	p.Close()
	if len(p.items) != 0 {
		t.Fatalf("Close 后 items 应当为空，实际 len=%d", len(p.items))
	}
}

func TestPoolLRUEviction(t *testing.T) {
	p := NewPool()
	defer p.Close()

	p.SetMaxConns(3)

	p.getOrCreate("user1@icloud.com")
	p.getOrCreate("user2@icloud.com")
	p.getOrCreate("user3@icloud.com")

	if len(p.items) != 3 {
		t.Fatalf("期望 3 个连接，实际 %d", len(p.items))
	}

	// 访问 user1 使其变为最近使用
	p.getOrCreate("user1@icloud.com")

	// 插入第 4 个连接，应当淘汰最久未使用的 user2
	p.getOrCreate("user4@icloud.com")

	if len(p.items) != 3 {
		t.Fatalf("超出上限后期望 3 个连接，实际 %d", len(p.items))
	}

	p.mu.Lock()
	_, hasUser1 := p.items["user1@icloud.com"]
	_, hasUser2 := p.items["user2@icloud.com"]
	_, hasUser3 := p.items["user3@icloud.com"]
	_, hasUser4 := p.items["user4@icloud.com"]
	p.mu.Unlock()

	if !hasUser1 || !hasUser3 || !hasUser4 {
		t.Fatalf("保留的应为 user1, user3, user4")
	}
	if hasUser2 {
		t.Fatalf("user2 应当被 LRU 驱逐")
	}
}
