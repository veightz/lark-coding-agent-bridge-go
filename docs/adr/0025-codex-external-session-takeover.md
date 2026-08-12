# ADR-0025: Codex 外部会话状态与安全接管

- 状态: accepted
- 日期: 2026-08-12
- 编号说明: 原并行分支写作 ADR-0022；合入 main 时 0022 已被 Cursor ACP 占用，顺延为本编号。

## 背景（Context）

Codex 会话可能先从 CLI 或桌面客户端启动。用户离开电脑后，希望在飞书查看
最近进度，并在需要时继续输入。现有 `/sessions`、`/bind` 只能扫描会话 ID、cwd
和首条用户消息；它们不展示最近输出和回合状态，也没有直观的 `/resume` 入口。

更重要的是，当前 Codex 适配器每轮独立启动 `codex app-server --stdio`。若另一个
CLI/桌面进程仍在写同一 thread，第二个 app-server 直接 `turn/start` 会形成并发
接管，不能把“文件可发现”误当成“活动回合可安全控制”。

Codex 新版 app-server 已提供 `thread/list/read` 的 runtime status、
`thread/resume` 和活动回合 `turn/steer(expectedTurnId)`；本机共享 daemon 可通过
`codex app-server proxy` 让多个客户端连接同一运行时。

## 选项（Options）

1. 继续只做 JSONL 扫描与 `/bind` — 实现最少，但用户看不到状态，活动回合还有
   并发写入风险。
2. 直接引入 AskHuman — 能让 agent 主动向飞书提问，但不能列出或接管任意既有
   Codex 会话，且会重复本仓库已有 ask broker / 飞书卡片能力。
3. 状态快照 + 安全续接；共享 daemon 时支持同回合 steer，非共享运行时拒绝
   活动会话并发接管。

## 决策（Decision）

选 3：

1. **状态快照**：Codex 会话发现器读取 JSONL 头部元数据与尾部事件，补充
   `active / idle` 状态和最近一条 agent 输出。`/sessions` 展示状态、最近输出、
   cwd 与更新时间。该快照对 CLI、桌面和 bridge 来源都有效。
2. **续接入口**：新增 `/resume <序号或 id 前缀>`，语义等同于显式绑定当前飞书
   scope；私聊回复落在该命令形成的话题内，后续在话题中续聊。
3. **安全边界**：磁盘显示回合仍 active 时，独立 stdio app-server 不启动新 turn，
   明确提示等待原回合结束或启用共享 daemon，避免两个进程同时推进同一 thread。
4. **共享 daemon（Codex 专用、显式启用）**：profile 的 `agent.appServerMode` 设为
   `daemon` 时，bridge 确保本机 daemon 已启动，再通过 `app-server proxy` 连接。
   `thread/resume` 返回 active 且能解析当前 in-progress turn 时，用
   `turn/steer(expectedTurnId)` 追加用户输入；idle 时仍用 `turn/start`。
5. **AskHuman 定位**：不把 `Naituw/AskHuman` 作为运行时依赖。它适合作为
   “agent 主动找人”的可选通道；本需求的主路径仍由会话发现/恢复协议完成。后续如需
   外部 Codex 主动提问，优先复用 `internal/ask`，提供兼容 AskHuman 使用习惯的
   bridge 子命令，而不是再部署一套飞书 bot 与问答状态机。

## 理由（Rationale）

- 观察与控制分离：JSONL 能提供跨客户端 best-effort 状态，但只有同一 app-server
  runtime 才能安全 steer 活动 turn。
- 显式拒绝不安全接管，优于静默制造同一 thread 的并发 turn。
- `/resume` 保留 `/bind` 的持久化与去重语义，不新增第二套绑定状态。
- AskHuman 的价值保留在主动通知层，不与本仓库现有 ask broker 重复建设。

## 后果（Consequences）

- JSONL 的 `active` 是 best-effort 快照；进程崩溃可能留下无 `task_complete` 的
  假 active。用户可显式等待/重试，后续可增加 stale 阈值与进程核验。
- `appServerMode=daemon` 只能 steer 同一 daemon 中实际 loaded 的 active thread；
  普通独立 CLI/TUI 不会被强行劫持。
- 共享 daemon 是 opt-in，不改变既有 profile 的默认进程模型。
- Codex Remote 已能在 ChatGPT 手机端查看、指导和审批本机任务；本功能提供的是
  飞书内入口，两者可以并存。
