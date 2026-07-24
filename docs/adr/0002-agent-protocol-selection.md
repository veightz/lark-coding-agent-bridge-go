# ADR-0002: 四种 agent 的通信协议选型（pi RPC / opencode serve，ACP 暂不迁移）

- 状态: accepted
- 日期: 2026-07-25

## 背景（Context）

桥接器需要为每个 agent 选一条机器通信通道：claude/codex 已用 spawn-per-message
的 stream-json/JSONL；新增 pi 与 opencode 时，owner 要求评估"是否天然支持通信"
以及 ACP（Agent Client Protocol）是否更优。调研结论（本机实测 + 官方文档）：

- pi 0.82：原生 `--mode rpc`（常驻进程、stdin JSONL 命令、stdout 流式事件、
  abort 优雅中断、steer/followUp 排队、--session-id 持久会话）；无原生 ACP，
  社区 adapter 有 MUST 级缺口。
- opencode 1.18：`serve`（REST + 全局 SSE 总线，单进程多 session，abort API，
  /global/health，session 落盘）；`acp`（原生 ACP server）；`run --format json`
  （无增量、不可中断，不适合）。
- ACP 生态：opencode 原生；claude/codex 经 Zed 维护的 adapter；协议本身适合
  桥接（阻塞 prompt + update 通知流、权限可自动应答），但 adapter 保真度参差。

## 选项（Options）

1. 全部迁 ACP（4/4）— 一套客户端代码，但 pi 无可用实现，claude/codex 需引 adapter 依赖。
2. 按 agent 选最优原生协议，ACP 作预留路径 — pi RPC、opencode serve、claude/codex 维持。
3. opencode 用 ACP（原生）、pi 用 RPC — opencode 官方 ACP，但多 session/健康检查不如 serve。

## 决策（Decision）

选 2：pi 用原生 RPC 常驻进程；opencode 用 serve（HTTP+SSE）；claude/codex 暂留
spawn-per-message；仓库内保留最小 ACP client（internal/agent/acp.go）作为
claude/codex 未来迁移的路径，适配器事件抽象与 ACP 语义对齐（超集）。

## 理由（Rationale）

- opencode serve 的多会话是一等公民（单进程多 session + 全局 SSE 按 sessionID
  分发），且 /global/health 直接服务三层健壮性（ADR-0003）；ACP 是单 client ↔
  单 agent 子进程模型，IDE 语义（plan/locations/terminal）对聊天桥是噪音。
- pi RPC 是其官方 machine interface，信息最全（rcarmo/vibes 亦同选）。
- 不强迁 ACP：避免为 claude/codex 引入 npm adapter 运行时依赖与翻译层保真风险。

## 后果（Consequences）

- 每个 agent 一套翻译器（表驱动单测兜底）；新 agent 接入成本 = 一个 Adapter 实现。
- 若 pi 未来原生支持 ACP 或 claude-agent-acp 足够稳定，可用 acp.go 平滑迁移，
  届时写新 ADR supersedes 本文档。

## 实测补充（opencode serve 的工作区路由）

v1.18 实测确认：`?directory=X` 创建的会话，其事件**只出现在**
`GET /event?directory=X` 这条工作区级流上，不在全局 `/event` 总线里
（全局流只有 server.connected/heartbeat 与服务器 cwd 下的会话事件）。
因此适配器按目录维护一条 SSE 订阅（懒启动 + 指数退避重连），
prompt/abort 调用同样携带 `?directory=`。
