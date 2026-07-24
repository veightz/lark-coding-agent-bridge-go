// Package supervisor keeps the Lark WebSocket connection healthy without
// human intervention. The SDK auto-reconnects on clean drops; this layer
// handles the nasty cases — half-dead connections that still look open:
// a periodic REST liveness probe, and a forced full reconnect when the
// probe keeps failing. It also exposes connection state for observability.
package supervisor

import (
	"context"
	"log"
	"sync"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// State describes the current connection health.
type State string

const (
	StateConnecting   State = "connecting"
	StateConnected    State = "connected"
	StateReconnecting State = "reconnecting"
	StateDown         State = "down"
)

// Status is a point-in-time snapshot for dashboards.
type Status struct {
	State            State     `json:"state"`
	StartedAt        time.Time `json:"startedAt"`
	LastEventAt      time.Time `json:"lastEventAt,omitempty"`
	LastConnectedAt  time.Time `json:"lastConnectedAt,omitempty"`
	ConsecutiveFails int       `json:"consecutiveProbeFails"`
	Restarts         int       `json:"restarts"`
}

// Config wires the supervisor.
type Config struct {
	AppID     string
	AppSecret string
	Domain    string
	Events    *dispatcher.EventDispatcher
	// Probe is a cheap REST call proving the app credentials + network work
	// (e.g. bot info). Called every ProbeInterval.
	Probe         func(ctx context.Context) error
	ProbeInterval time.Duration // default 60s
	ProbeTimeout  time.Duration // default 10s
	MaxProbeFails int           // default 3 — beyond this, force reconnect
	HandshakeWait time.Duration // default 15s — wait for OnReady after (re)start
	OnStateChange func(State)
}

// Supervisor owns the WS client lifecycle.
type Supervisor struct {
	cfg Config

	mu        sync.Mutex
	status    Status
	client    *larkws.Client
	stopCh    chan struct{}
	stopped   bool
	eventMark chan struct{}
}

// New builds a supervisor; call Run to start.
func New(cfg Config) *Supervisor {
	if cfg.ProbeInterval <= 0 {
		cfg.ProbeInterval = 60 * time.Second
	}
	if cfg.ProbeTimeout <= 0 {
		cfg.ProbeTimeout = 10 * time.Second
	}
	if cfg.MaxProbeFails <= 0 {
		cfg.MaxProbeFails = 3
	}
	if cfg.HandshakeWait <= 0 {
		cfg.HandshakeWait = 15 * time.Second
	}
	return &Supervisor{
		cfg:       cfg,
		stopCh:    make(chan struct{}),
		eventMark: make(chan struct{}, 1),
		status:    Status{State: StateConnecting, StartedAt: time.Now()},
	}
}

// MarkEvent records inbound activity (call from message handlers).
func (s *Supervisor) MarkEvent() {
	s.mu.Lock()
	s.status.LastEventAt = time.Now()
	s.mu.Unlock()
	select {
	case s.eventMark <- struct{}{}:
	default:
	}
}

// Status returns a snapshot of the connection state.
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// Stop shuts the supervisor and the WS client down.
func (s *Supervisor) Stop() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	client := s.client
	s.mu.Unlock()
	close(s.stopCh)
	if client != nil {
		client.Close()
	}
}

// Run starts the connect-supervise-reconnect loop and blocks until Stop.
func (s *Supervisor) Run(ctx context.Context) {
	for {
		if s.isStopped() {
			return
		}
		s.connectOnce(ctx)

		if s.isStopped() {
			return
		}
		// connectOnce returned: the client died or the probe forced a
		// restart. Back off briefly and rebuild.
		s.setState(StateDown)
		select {
		case <-s.stopCh:
			return
		case <-time.After(3 * time.Second):
		}
		s.mu.Lock()
		s.status.Restarts++
		s.mu.Unlock()
	}
}

// connectOnce starts one WS client and supervises it until it dies or the
// liveness probe forces a restart.
func (s *Supervisor) connectOnce(ctx context.Context) {
	s.setState(StateConnecting)

	ready := make(chan struct{})
	var readyOnce sync.Once
	client := larkws.NewClient(s.cfg.AppID, s.cfg.AppSecret,
		larkws.WithEventHandler(s.cfg.Events),
		larkws.WithDomain(s.cfg.Domain),
		larkws.WithLogLevel(larkcore.LogLevelWarn),
		larkws.WithOnReady(func() {
			readyOnce.Do(func() { close(ready) })
		}),
		larkws.WithOnReconnecting(func() {
			log.Println("[supervisor] ws 重连中…")
			s.setState(StateReconnecting)
		}),
		larkws.WithOnReconnected(func() {
			log.Println("[supervisor] ws 已重连")
			s.setState(StateConnected)
		}),
		larkws.WithOnDisconnected(func() {
			log.Println("[supervisor] ws 断开")
			s.setState(StateDown)
		}),
		larkws.WithOnError(func(err error) {
			log.Printf("[supervisor] ws 错误: %v", err)
		}),
	)

	s.mu.Lock()
	s.client = client
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		// Start blocks forever on success (internal select{}); a return
		// means the connection failed beyond SDK-level recovery.
		_ = client.Start(ctx)
		close(done)
	}()

	// Wait for the first successful handshake.
	select {
	case <-ready:
		s.setState(StateConnected)
		s.mu.Lock()
		s.status.LastConnectedAt = time.Now()
		s.status.ConsecutiveFails = 0
		s.mu.Unlock()
		log.Println("[supervisor] ws 已连接")
	case <-done:
		log.Println("[supervisor] ws 启动失败，准备重试")
		return
	case <-time.After(s.cfg.HandshakeWait):
		log.Println("[supervisor] ws 握手超时，重建连接")
		client.Close()
		return
	case <-s.stopCh:
		client.Close()
		return
	}

	// Supervision loop: periodic REST probe; too many consecutive failures
	// means the WS is half-dead — tear it down and reconnect from scratch.
	ticker := time.NewTicker(s.cfg.ProbeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			client.Close()
			return
		case <-done:
			log.Println("[supervisor] ws 进程退出，准备重建")
			return
		case <-ticker.C:
			probeCtx, cancel := context.WithTimeout(ctx, s.cfg.ProbeTimeout)
			err := s.cfg.Probe(probeCtx)
			cancel()

			s.mu.Lock()
			if err != nil {
				s.status.ConsecutiveFails++
			} else {
				s.status.ConsecutiveFails = 0
			}
			fails := s.status.ConsecutiveFails
			s.mu.Unlock()

			if err != nil {
				log.Printf("[supervisor] 存活探测失败 (%d/%d): %v", fails, s.cfg.MaxProbeFails, err)
			}
			if fails >= s.cfg.MaxProbeFails {
				log.Println("[supervisor] 存活探测连续失败，强制重建 WS 连接")
				client.Close()
				return
			}
		}
	}
}

func (s *Supervisor) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

func (s *Supervisor) setState(state State) {
	s.mu.Lock()
	changed := s.status.State != state
	s.status.State = state
	s.mu.Unlock()
	if changed && s.cfg.OnStateChange != nil {
		s.cfg.OnStateChange(state)
	}
}
