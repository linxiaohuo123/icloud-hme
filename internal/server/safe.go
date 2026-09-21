/**
 * [INPUT]: 依赖 log, sync
 * [OUTPUT]: 对外提供 goSafe, runBounded
 * [POS]: internal/server 的派生 goroutine 兜底与有界并发工具层
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"log"
	"sync"
)

// goSafe 启动一个带 panic 兜底的 goroutine。
//
// 【设计红线】gin.Recovery() 只覆盖 HTTP 处理链路；任何 `go func()` 里未捕获的
// panic 都会直接终止整个进程。因此 server 包内所有派生 goroutine 必须经由本函数
// (或自带等价 recover)，否则一个 worker 的越界/nil 解引用就会把全站打死。
func goSafe(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[PANIC RECOVER] %s: %v", name, r)
			}
		}()
		fn()
	}()
}

// runBounded 以最多 concurrency 个并发执行全部任务并等待结束。
// 既防止千号场景下无限 fan-out 打爆 fd/TLS 客户端，也保证每个任务 panic 不会拖垮进程。
func runBounded(concurrency int, name string, tasks []func()) {
	if concurrency < 1 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, task := range tasks {
		wg.Add(1)
		sem <- struct{}{}
		t := task
		goSafe(name, func() {
			defer wg.Done()
			defer func() { <-sem }()
			t()
		})
	}
	wg.Wait()
}
