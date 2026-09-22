/**
 * [INPUT]: 依赖 testing, fmt, sync
 * [OUTPUT]: 对外提供 TestClaimPoolAlias, TestClaimPoolAliasConcurrency
 * [POS]: internal/store 的预热池别名认领单元测试，验证原子认领与并发去重正确性
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"fmt"
	"sync"
	"testing"
)

func TestClaimPoolAlias(t *testing.T) {
	tempDir := t.TempDir()

	s, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer s.Close()

	// 1. 模拟 scheduler 预先补货了 2 个别名
	_ = s.RecordLease(LeaseRecord{
		Email:     "pool1@icloud.com",
		AccountID: "acc_1",
		Tag:       "t1",
		TokenName: "scheduler",
	})
	_ = s.RecordLease(LeaseRecord{
		Email:     "pool2@icloud.com",
		AccountID: "acc_1",
		Tag:       "t1",
		TokenName: "scheduler",
	})

	candidates := []PoolCandidate{
		{AccountID: "acc_1", Email: "pool1@icloud.com"},
		{AccountID: "acc_1", Email: "pool2@icloud.com"},
		{AccountID: "acc_1", Email: "fresh_apple_web@icloud.com"},
	}

	// 2. 统计初始可用数 (3 个)
	avail, err := s.CountAvailablePoolAliases(candidates)
	if err != nil || avail != 3 {
		t.Fatalf("CountAvailablePoolAliases 期望 3, 实际: %d (err=%v)", avail, err)
	}

	// 3. 第一次认领：应拿到 pool1
	rec1, err := s.ClaimPoolAlias(candidates, "t1", "token", "tok_alpha", "bot_alpha")
	if err != nil || rec1 == nil || rec1.Email != "pool1@icloud.com" {
		t.Fatalf("ClaimPoolAlias 1 期望 pool1, 实际: %+v, err: %v", rec1, err)
	}

	// 4. 第二次认领：应拿到 pool2
	rec2, err := s.ClaimPoolAlias(candidates, "t1", "token", "tok_beta", "bot_beta")
	if err != nil || rec2 == nil || rec2.Email != "pool2@icloud.com" {
		t.Fatalf("ClaimPoolAlias 2 期望 pool2, 实际: %+v, err: %v", rec2, err)
	}

	// 5. 第三次认领：应拿到 fresh_apple_web
	rec3, err := s.ClaimPoolAlias(candidates, "t1", "token", "tok_gamma", "bot_gamma")
	if err != nil || rec3 == nil || rec3.Email != "fresh_apple_web@icloud.com" {
		t.Fatalf("ClaimPoolAlias 3 期望 fresh_apple_web, 实际: %+v, err: %v", rec3, err)
	}

	// 6. 第四次认领：池已空，应返回 nil
	rec4, err := s.ClaimPoolAlias(candidates, "t1", "token", "tok_delta", "bot_delta")
	if err != nil || rec4 != nil {
		t.Fatalf("ClaimPoolAlias 4 期望 nil, 实际: %+v, err: %v", rec4, err)
	}

	// 7. 再次统计可用数 (0 个)
	availAfter, _ := s.CountAvailablePoolAliases(candidates)
	if availAfter != 0 {
		t.Fatalf("CountAvailablePoolAliases 期望 0, 实际: %d", availAfter)
	}
}

func TestClaimPoolAliasConcurrency(t *testing.T) {
	tempDir := t.TempDir()

	s, err := NewStore(tempDir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer s.Close()

	const totalAliases = 30
	candidates := make([]PoolCandidate, totalAliases)
	for i := 0; i < totalAliases; i++ {
		candidates[i] = PoolCandidate{
			AccountID: "acc_1",
			Email:     fmt.Sprintf("alias_%d@icloud.com", i),
		}
	}

	// 30 个并发协程抢领 30 个别名，必须恰好人手一个，绝无重复
	const numGoroutines = 30
	claimedMap := sync.Map{}
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(workerID int) {
			defer wg.Done()
			rec, err := s.ClaimPoolAlias(candidates, "test_tag", "token", fmt.Sprintf("tok_%d", workerID), fmt.Sprintf("bot_%d", workerID))
			if err != nil {
				t.Errorf("worker %d 报错: %v", workerID, err)
				return
			}
			if rec != nil {
				if _, loaded := claimedMap.LoadOrStore(rec.Email, workerID); loaded {
					t.Errorf("别名 %s 被重复认领！", rec.Email)
				}
			}
		}(i)
	}
	wg.Wait()

	count := 0
	claimedMap.Range(func(key, value any) bool {
		count++
		return true
	})
	if count != totalAliases {
		t.Fatalf("并发认领期望领取 %d 个唯一别名，实际仅领取 %d 个", totalAliases, count)
	}

	consumed := s.CountConsumedPoolAliases()
	if consumed != totalAliases {
		t.Fatalf("CountConsumedPoolAliases 期望 %d, 实际: %d", totalAliases, consumed)
	}
}
