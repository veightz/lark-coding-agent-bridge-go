// Package registry maintains a local process registry so the dashboard
// command can see every bridge instance: profile, agent, binary version,
// workspace and liveness. Each bridge process registers itself on start,
// heartbeats periodically, and deregisters on exit; stale entries are
// pruned by readers via PID liveness probes.
package registry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"lark-coding-agent-bridge-go/internal/buildinfo"
	"lark-coding-agent-bridge-go/internal/config"
)

// Entry is one running (or stale) bridge process.
type Entry struct {
	PID         int            `json:"pid"`
	Profile     string         `json:"profile"`
	Agent       string         `json:"agent"`
	Binary      string         `json:"binary"`
	Workspace   string         `json:"workspace"`
	Version     buildinfo.Info `json:"version"`
	StartedAt   time.Time      `json:"startedAt"`
	HeartbeatAt time.Time      `json:"heartbeatAt"`
}

func registryPath(paths config.Paths) string {
	return filepath.Join(paths.Home, "registry", "processes.json")
}

// acquireRegistryLock 打开 registry 目录下的锁文件并获取排它 flock。
// 多实例同时启动时，各自 upsert 的 load→save 序列需要串行化，否则并发
// 读写会互相覆盖导致 registry 丢记录（dashboard 看不到部分实例）。
func acquireRegistryLock(paths config.Paths) (*os.File, error) {
	dir := filepath.Join(paths.Home, "registry")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// withLock 在持有注册表文件锁期间执行 fn，保证读改写原子完成。
func withLock(paths config.Paths, fn func() error) error {
	f, err := acquireRegistryLock(paths)
	if err != nil {
		return err
	}
	defer func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}()
	return fn()
}

func load(paths config.Paths) ([]Entry, error) {
	data, err := os.ReadFile(registryPath(paths))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, nil // corrupt registry: start over rather than crash
	}
	return entries, nil
}

func save(paths config.Paths, entries []Entry) error {
	return config.WriteJSONAtomic(registryPath(paths), entries)
}

// upsert writes or refreshes this process's entry.
func upsert(paths config.Paths, e Entry) error {
	return withLock(paths, func() error {
		entries, _ := load(paths)
		found := false
		for i := range entries {
			if entries[i].PID == e.PID {
				entries[i] = e
				found = true
				break
			}
		}
		if !found {
			entries = append(entries, e)
		}
		return save(paths, entries)
	})
}

// Register adds this process and starts the heartbeat goroutine.
// The returned func deregisters (call on shutdown).
func Register(paths config.Paths, profile, agentKind, workspace string) func() {
	self, _ := os.Executable()
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	entry := Entry{
		PID:         os.Getpid(),
		Profile:     profile,
		Agent:       agentKind,
		Binary:      self,
		Workspace:   workspace,
		Version:     buildinfo.Current(),
		StartedAt:   time.Now(),
		HeartbeatAt: time.Now(),
	}
	_ = upsert(paths, entry)

	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				entry.HeartbeatAt = time.Now()
				_ = upsert(paths, entry)
			}
		}
	}()

	return func() {
		close(stop)
		_ = withLock(paths, func() error {
			entries, _ := load(paths)
			out := entries[:0]
			for _, e := range entries {
				if e.PID != entry.PID {
					out = append(out, e)
				}
			}
			return save(paths, out)
		})
	}
}

// Alive reports whether the entry's process exists.
func (e Entry) Alive() bool {
	proc, err := os.FindProcess(e.PID)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// List returns all registered entries, pruning dead ones, sorted by start time.
func List(paths config.Paths) ([]Entry, error) {
	entries, err := load(paths)
	if err != nil {
		return nil, err
	}
	alive := entries[:0]
	changed := false
	for _, e := range entries {
		if e.Alive() {
			alive = append(alive, e)
		} else {
			changed = true
		}
	}
	if changed {
		_ = save(paths, alive)
	}
	sort.Slice(alive, func(i, j int) bool { return alive[i].StartedAt.Before(alive[j].StartedAt) })
	return alive, nil
}
