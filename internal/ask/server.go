package ask

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

// ScopeResolver looks up chat routing for a hook-originated ask.
type ScopeResolver func(scope string) (Route, bool)

// Server is a loopback HTTP endpoint for Claude (and other) hooks.
// POST /v1/ask  body: {scope, questions?, raw?}  → blocks until answered.
// GET  /health  → 200 ok
type Server struct {
	Broker   *Broker
	Resolve  ScopeResolver
	Listener net.Listener

	mu     sync.Mutex
	server *http.Server
}

// StartListen binds 127.0.0.1:0 and serves until ctx cancel / Close.
func (s *Server) StartListen() (url string, err error) {
	if s.Broker == nil {
		return "", fmt.Errorf("ask server: broker required")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	s.Listener = ln
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/v1/ask", s.handleAsk)
	s.server = &http.Server{Handler: mux}
	go func() {
		if err := s.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[ask] http server: %v", err)
		}
	}()
	return "http://" + ln.Addr().String(), nil
}

// Close shuts down the HTTP server.
func (s *Server) Close() error {
	if s.server == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.server.Shutdown(ctx)
}

// URL is the base URL after StartListen.
func (s *Server) URL() string {
	if s.Listener == nil {
		return ""
	}
	return "http://" + s.Listener.Addr().String()
}

type askRequestBody struct {
	Scope     string     `json:"scope"`
	ChatID    string     `json:"chatId"`
	ReplyTo   string     `json:"rootMessageId"`
	Source    string     `json:"source"`
	TimeoutMs int        `json:"timeoutMs"`
	Questions []Question `json:"questions"`
	// Raw Claude hook payload (optional) for format round-trip.
	Raw json.RawMessage `json:"raw"`
}

type askResponseBody struct {
	Kind    string     `json:"kind"`
	Answers [][]string `json:"answers,omitempty"`
	By      string     `json:"by,omitempty"`
	Comment string     `json:"comment,omitempty"`
	Reason  string     `json:"reason,omitempty"`
	// Claude directive when source is claude-hook and we have raw questions.
	Directive string `json:"directive,omitempty"`
}

func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var req askRequestBody
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	route := Route{ChatID: req.ChatID, ReplyTo: req.ReplyTo, Scope: req.Scope}
	if route.ChatID == "" && req.Scope != "" && s.Resolve != nil {
		if rt, ok := s.Resolve(req.Scope); ok {
			route = rt
			if req.Scope != "" {
				route.Scope = req.Scope
			}
		}
	}
	if route.ChatID == "" {
		http.Error(w, "no chat route for scope", http.StatusConflict)
		return
	}

	questions := req.Questions
	var rawQuestions []map[string]any
	eventName := "PreToolUse"
	if len(questions) == 0 && len(req.Raw) > 0 {
		qs, rawQs, err := ParseClaudeHookPayload(req.Raw)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if qs == nil {
			// Not AskUserQuestion → tell client to passthrough.
			writeJSON(w, http.StatusOK, askResponseBody{Kind: "passthrough"})
			return
		}
		questions = qs
		rawQuestions = rawQs
		var p map[string]any
		_ = json.Unmarshal(req.Raw, &p)
		if e, _ := p["hook_event_name"].(string); e != "" {
			eventName = e
		}
	}
	if len(questions) == 0 {
		http.Error(w, "questions required", http.StatusBadRequest)
		return
	}

	source := req.Source
	if source == "" {
		source = "hook"
	}
	timeout := time.Duration(req.TimeoutMs) * time.Millisecond
	result, err := s.Broker.Register(CreateInput{
		Route:      route,
		Questions:  questions,
		Timeout:    timeout,
		Source:     source,
		RawPayload: req.Raw,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := askResponseBody{
		Kind:    string(result.Kind),
		Answers: result.Answers,
		By:      result.By,
		Comment: result.Comment,
		Reason:  result.Reason,
	}
	if len(rawQuestions) > 0 {
		// Always return a directive so Claude unblocks. On timeout/cancel
		// fill answers with a short status string (Claude treats it as the
		// user's reply text).
		if result.Kind != KindAnswered {
			if result.Comment == "" {
				switch result.Kind {
				case KindTimedOut:
					result.Comment = "（飞书侧超时未作答）"
				default:
					result.Comment = "（提问已取消：" + result.Reason + "）"
				}
			}
		}
		resp.Directive = FormatClaudeAnswer(eventName, rawQuestions, questions, result)
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
