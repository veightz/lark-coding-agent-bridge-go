package registry

import (
	"fmt"
	"sync"
	"testing"

	"lark-coding-agent-bridge-go/internal/config"
)

// TestConcurrentUpsertNoLoss 验证多进程并发 upsert 时不会互相覆盖丢记录。
// 修复前：load→save 无锁，并发启动的多个实例会丢失彼此的 registry 记录。
func TestConcurrentUpsertNoLoss(t *testing.T) {
	paths := config.NewPaths()
	// 用临时 HOME 指向临时目录，避免污染真实 registry
	paths.Home = t.TempDir()

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := Entry{
				PID:     1000 + i,
				Profile: fmt.Sprintf("profile-%d", i),
			}
			if err := upsert(paths, e); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("upsert 失败: %v", err)
	}

	entries, err := load(paths)
	if err != nil {
		t.Fatalf("load 失败: %v", err)
	}
	if len(entries) != n {
		t.Fatalf("期望 %d 条记录，实际 %d 条（存在并发覆盖）", n, len(entries))
	}
	seen := map[int]bool{}
	for _, e := range entries {
		seen[e.PID] = true
	}
	for i := 0; i < n; i++ {
		if !seen[1000+i] {
			t.Fatalf("记录 PID %d 丢失", 1000+i)
		}
	}
}
