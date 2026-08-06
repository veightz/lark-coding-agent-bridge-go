package bridge

import (
	"fmt"
	"strings"

	"lark-coding-agent-bridge-go/internal/agent"
)

func (b *Bridge) handleMode(scope string, args []string, reply func(string)) {
	provider, ok := b.Agent.(agent.CollaborationModeProvider)
	if !ok {
		reply("当前 agent（" + b.Agent.DisplayName() + "）暂不支持 /mode。")
		return
	}

	if len(args) == 0 {
		var choices []string
		for _, mode := range provider.CollaborationModes() {
			choices = append(choices, "`/mode "+string(mode.ID)+"` — "+mode.DisplayName+"："+mode.Description)
		}
		reply("当前协作模式：`" + string(b.currentCollaborationMode(scope)) + "`\n\n" + strings.Join(choices, "\n"))
		return
	}
	if len(args) != 1 {
		reply("用法：`/mode plan` 或 `/mode default`")
		return
	}

	want := agent.CollaborationMode(strings.ToLower(args[0]))
	err := b.setCollaborationMode(scope, provider, want)
	if err != nil {
		reply("⚠️ " + err.Error())
		return
	}
	if want == agent.CollaborationModePlan {
		reply("✅ 已切换为 Plan 模式。后续消息会先检查、澄清并给出方案，不直接实施；确认后发送 `/mode default` 恢复执行。")
		return
	}
	reply("✅ 已切换为 Default 模式，后续消息可正常执行代码变更。")
}

func (b *Bridge) currentCollaborationMode(scope string) agent.CollaborationMode {
	if sess, ok := b.Sessions.Get(scope); ok && sess.CollaborationMode != "" {
		return agent.CollaborationMode(sess.CollaborationMode)
	}
	return agent.CollaborationModeDefault
}

func (b *Bridge) setCollaborationMode(scope string, provider agent.CollaborationModeProvider, want agent.CollaborationMode) error {
	var selected agent.CollaborationModeInfo
	for _, mode := range provider.CollaborationModes() {
		if mode.ID == want {
			selected = mode
			break
		}
	}
	if selected.ID == "" {
		return fmt.Errorf("不支持协作模式 %q；可用值：plan、default", want)
	}

	b.stopRun(scope)
	sess, _ := b.Sessions.Get(scope)
	sess.CollaborationMode = string(want)
	b.Sessions.Set(scope, sess)
	if err := b.Sessions.Flush(); err != nil {
		return fmt.Errorf("保存协作模式失败：%w", err)
	}
	return nil
}
