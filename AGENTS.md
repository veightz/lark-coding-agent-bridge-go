# AGENTS.md — 给所有 coding agent 的入口说明

任何 agent（Claude Code、Codex、pi、opencode、Kimi 等）接手本仓库前，先读完本文件，再按指针阅读 `docs/`。

## 这个项目是什么

飞书/Lark ↔ 本地 coding agent CLI 的桥接器（Go）。把聊天消息路由到本地 agent（claude / codex / pi / opencode），回复以 CardKit 流式卡片实时呈现。是 TS 项目 [lark-channel-bridge](https://github.com/zarazhangrui/lark-coding-agent-bridge) 的 Go 复刻增强版。

## 重点学习参考

- **[deepcoldy/botmux](https://github.com/deepcoldy/botmux)**（**首选对照**）：飞书遥控 AI 编程 CLI 的成熟实现。本仓库在以下能力上重点对齐其设计与语义，而非照搬进程模型：
  - **模型反问接管**：Claude `AskUserQuestion` / OpenCode `question` / Pi `extension_ui_request` → 飞书交互卡片 → 用户点选（或文字 freeform）后回填 agent（见 ADR-0008、`internal/ask`，对照 botmux `ask-broker` / `ask-card` / hook-installer）。
  - **OpenCode 工具权限**（ADR-0011）：`defaultAccess=full`（默认）时 auto-reply `always`；`workspace`/`read-only` 时弹飞书卡（允许一次 / 始终允许 / 拒绝）→ `POST /permission/{id}/reply`。
  - 卡片回调、超时 settle、daemon 不可达时 passthrough 降级等产品细节。
  - 文档入口：botmux README、`docs/TEST-GUIDE-ask-hooks.md`、源码 `src/core/ask-*.ts`、`src/im/lark/ask-card.ts`。
- [lark-channel-bridge](https://github.com/zarazhangrui/lark-coding-agent-bridge)：原版 TS 桥接器（多 profile、流式卡片、命令体系的起点）。

## 工作模式：设计驱动开发（Design-Driven Development）

本仓库按 **设计 → 实现 → 测试** 的循环推进，设计沉淀在仓库里而非任何 agent 的记忆里：

1. **改架构/协议/关键流程前**，先更新或新增设计文档：
   - 新的重要决策 → 在 `docs/adr/` 新增 ADR（编号递增，不修改已定稿的旧 ADR；推翻旧决策就写新 ADR 标注 supersedes）。
   - 行为变化 → 同步更新 `docs/design.html`（技术设计）与 `docs/implementation.html`（实现层结构/方法）。
2. **实现**：最小改动，风格与周边代码一致。
3. **测试**：改动的逻辑必须有测试覆盖（`go test ./...` 全绿才算完成）。真实 CLI 冒烟用 `LARK_LIVE_TEST=1 go test ./internal/agent -run Live -v`。
4. **收尾**：检查 AGENTS.md 是否仍准确（命令、结构变化要同步）。

## 构建与验证

```bash
go build ./...                 # 编译
go test ./...                  # 全部测试（必须全绿）
go vet ./... && gofmt -l .     # 静态检查（必须无输出）
go build -o bin/lark-coding-agent-bridge ./cmd/lark-coding-agent-bridge
```

CLI：`run`（默认）/ `dashboard` / `upgrade [--check]` / `version`。

## 仓库地图

```
cmd/lark-coding-agent-bridge/   CLI 入口与子命令
internal/
  onboard/      扫码注册向导（registerApp 协议复刻）
  config/       根配置 + 路径布局（~/.lark-coding-agent-bridge/）
  state/        会话 / 工作区 / 绑定 JSON 存储
  agent/        四种 agent 适配器 + ACP client（预留）
  ask/          模型反问 broker + 飞书 ask 卡片 + Claude hook IPC（ADR-0008，对标 botmux）
  lark/         OpenAPI 封装（WS、REST、cardkit、附件）
  card/         运行状态机 + 卡片渲染 + 流式更新
  bridge/       消息路由、防抖队列、斜杠命令、访问控制入口、显式建群、ask 接线
  policy/       聊天访问决策（owner / invite 名单，ADR-0013）
  media/        附件下载缓存
  supervisor/   WS 连接监督（探测 + 强制重建）
  registry/     进程注册表（dashboard 数据源）
  upgrade/      自升级（git pull + 重建 + 原子替换）
  buildinfo/    版本信息（debug.ReadBuildInfo）
docs/
  README.md           文档体系说明（先读）
  design.html         技术设计（架构图、时序图、状态机）
  implementation.html 实现层（关键结构与方法）
  adr/                架构决策记录（Architecture Decision Records）
```

## 约定

- 不引入未经确认的新依赖；优先标准库。当前第三方依赖仅 `oapi-sdk-go` 与 `qrterminal`。
- 配置/状态文件一律原子写（`config.WriteJSONAtomic`）。
- 面向用户的消息用中文；代码注释用中文或英文均可，与所在文件保持一致。
- 敏感信息（App Secret 等）不打印、不进日志、不进测试断言。
- agent 适配器的事件抽象是 `agent.Event`；新增 agent 时实现 `Adapter`（+ 常驻型实现 `SessionResetter`），翻译逻辑必须有表驱动单测。

## 当前未实现（不要误以为是 bug）

后台服务管理、云文档评论、COT 消息、/resume、/config 卡片、secrets 加密、`/invite all group` 批量列举；
Pi 的 notify/setStatus 等 fire-and-forget UI 不弹卡；Claude 工具级权限仍走 access mode（不做飞书审批卡）；
Codex 反问与权限确认已通过 app-server 接管（ADR-0014）；OpenCode 工具权限已接管（ADR-0011）。
访问控制（owner + `/invite`/`/remove` 白名单）见 ADR-0013。
详见 `docs/adr/` 与 README「与原版的差异」。
