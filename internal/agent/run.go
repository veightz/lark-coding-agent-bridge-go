package agent

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// procRun is the shared Run implementation over an exec.Cmd child process.
type procRun struct {
	cmd    *exec.Cmd
	events chan Event

	mu         sync.Mutex
	stopped    bool
	exited     chan struct{}
	exitCode   int
	stopGrace  time.Duration
	cleanupFns []func()
}

// startProc launches cmd (stdin already carrying the prompt) and returns a
// procRun whose stdout lines are fed to translate in a background goroutine.
func startProc(cmd *exec.Cmd, prompt string, stopGraceMs int, env []string, translate func(line []byte) []Event, terminalErr func(msg string) Event) (*procRun, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	cmd.Env = env

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to spawn %s: %w", cmd.Path, err)
	}

	grace := time.Duration(stopGraceMs) * time.Millisecond
	if grace <= 0 {
		grace = 5 * time.Second
	}
	r := &procRun{
		cmd:       cmd,
		events:    make(chan Event, 64),
		exited:    make(chan struct{}),
		exitCode:  -1,
		stopGrace: grace,
	}

	// Write the prompt then close stdin; a dead child makes this fail quietly.
	go func() {
		_, _ = stdin.Write([]byte(prompt))
		_ = stdin.Close()
	}()

	go func() {
		defer close(r.events)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}
			for _, evt := range translate(append([]byte(nil), line...)) {
				r.emit(evt)
			}
		}
		waitErr := cmd.Wait()
		code := 0
		if waitErr != nil {
			var ee *exec.ExitError
			if errors.As(waitErr, &ee) {
				code = ee.ExitCode()
			} else {
				code = -1
			}
		}
		r.mu.Lock()
		r.exitCode = code
		wasStopped := r.stopped
		r.mu.Unlock()
		close(r.exited)

		for _, fn := range r.cleanupFns {
			fn()
		}

		if code != 0 && !wasStopped {
			detail := bytes.TrimSpace(stderr.Bytes())
			msg := fmt.Sprintf("%s exited with code %d", cmd.Args[0], code)
			if len(detail) > 0 {
				if len(detail) > 500 {
					detail = detail[:500]
				}
				msg += ": " + string(detail)
			}
			r.emit(terminalErr(msg))
		}
	}()

	return r, nil
}

func (r *procRun) emit(evt Event) {
	defer func() { _ = recover() }() // never panic on closed channel
	r.events <- evt
}

func (r *procRun) Events() <-chan Event { return r.events }

func (r *procRun) Stop() {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.stopped = true
	r.mu.Unlock()

	if r.cmd.Process == nil {
		return
	}
	_ = r.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-r.exited:
	case <-time.After(r.stopGrace):
		_ = r.cmd.Process.Kill()
		<-r.exited
	}
}

func (r *procRun) WaitExit(timeoutMs int) bool {
	select {
	case <-r.exited:
		return true
	case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
		return false
	}
}

func (r *procRun) onCleanup(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	select {
	case <-r.exited:
		fn()
	default:
		r.cleanupFns = append(r.cleanupFns, fn)
	}
}

// mergeEnv overlays overrides onto the current process environment.
func mergeEnv(overrides map[string]string) []string {
	env := os.Environ()
	for k, v := range overrides {
		env = append(env, k+"="+v)
	}
	return env
}
