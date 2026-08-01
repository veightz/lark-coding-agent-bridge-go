package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"lark-coding-agent-bridge-go/internal/config"
)

// ProfileLockedError reports that another bridge process already owns the
// profile. PID is best-effort metadata read from the lock file.
type ProfileLockedError struct {
	Profile string
	PID     int
}

func (e *ProfileLockedError) Error() string {
	if e.PID > 0 {
		return fmt.Sprintf("profile %q 已由 bridge 进程 PID %d 运行", e.Profile, e.PID)
	}
	return fmt.Sprintf("profile %q 已有 bridge 进程运行", e.Profile)
}

type profileLockMetadata struct {
	PID int `json:"pid"`
}

// ProfileLock is an OS-owned advisory lock held for the lifetime of one
// bridge process. The lock file remains on disk after release; only flock
// ownership indicates whether the profile is running.
type ProfileLock struct {
	file *os.File
	once sync.Once
}

// AcquireProfileLock obtains the non-blocking exclusive lock for profile.
// Different profiles use different files and may run concurrently.
func AcquireProfileLock(paths config.Paths, profile string) (*ProfileLock, error) {
	dir := paths.ProfileDir(profile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建 profile 目录: %w", err)
	}
	path := filepath.Join(dir, "bridge.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开 profile 锁: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, &ProfileLockedError{Profile: profile, PID: readProfileLockPID(path)}
		}
		return nil, fmt.Errorf("获取 profile 锁: %w", err)
	}

	metadata, _ := json.Marshal(profileLockMetadata{PID: os.Getpid()})
	if err := f.Truncate(0); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, fmt.Errorf("写入 profile 锁: %w", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, fmt.Errorf("写入 profile 锁: %w", err)
	}
	if _, err := f.Write(append(metadata, '\n')); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, fmt.Errorf("写入 profile 锁: %w", err)
	}
	return &ProfileLock{file: f}, nil
}

func readProfileLockPID(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var metadata profileLockMetadata
	if json.Unmarshal(data, &metadata) != nil {
		return 0
	}
	return metadata.PID
}

// Release gives up the profile lock. It is safe to call more than once.
func (l *ProfileLock) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
		_ = l.file.Close()
	})
}
