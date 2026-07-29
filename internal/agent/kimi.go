package agent

import (
	"encoding/json"
	"fmt"
	"os/exec"
)

// KimiAdapter drives the local `kimi` CLI (Kimi Code).
type KimiAdapter struct {
	binary      string
	botIdentity *BotIdentity
	Env         map[string]string
}

func (a *KimiAdapter) ID() string          { return "kimi" }
func (a *KimiAdapter) DisplayName() string { return "Kimi" }

func (a *KimiAdapter) SetBotIdentity(id BotIdentity) { a.botIdentity = &id }

func (a *KimiAdapter) Run(opts RunOptions) (Run, error) {
	if opts.Cwd == "" {
		return nil, fmt.Errorf("cwd is required for KimiAdapter.Run")
	}

	prompt := BuildSystemPrompt(a.botIdentity) + "\n\n" + opts.Prompt

	args := []string{
		"-p",
		prompt,
		"--output-format", "stream-json",
	}
	if opts.Model != "" {
		args = append(args, "-m", opts.Model)
	}
	if opts.SessionID != "" {
		args = append(args, "-S", opts.SessionID)
	}
	args = append(args, opts.ExtraArgs...)

	binary := a.binary
	if binary == "" {
		binary = "kimi"
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = opts.Cwd

	run, err := startProc(cmd, "", opts.StopGraceMs, mergeEnv(mergeEnvMaps(a.Env, opts.Env)), translateKimiLine, func(msg string) Event {
		return Event{Type: EventError, Message: msg, TerminationReason: TermFailed}
	})
	if err != nil {
		return nil, err
	}
	if opts.Model != "" {
		safeSend(run.events, Event{Type: EventSystem, Model: opts.Model})
	}
	return run, nil
}

type kimiRawEvent struct {
	Role      string `json:"role"`
	Content   string `json:"content,omitempty"`
	Type      string `json:"type,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Command   string `json:"command,omitempty"`
}

func translateKimiLine(line []byte) []Event {
	var raw kimiRawEvent
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil
	}

	switch raw.Role {
	case "assistant":
		if raw.Content != "" {
			return []Event{{Type: EventText, Delta: raw.Content}}
		}
	case "meta":
		if raw.SessionID != "" {
			return []Event{{Type: EventSystem, SessionID: raw.SessionID}}
		}
	}
	return nil
}
