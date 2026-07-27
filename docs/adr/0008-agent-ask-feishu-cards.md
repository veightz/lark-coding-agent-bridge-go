# ADR-0008: 模型反问 / 权限类提问以飞书交互卡片接管

- 状态: accepted
- 日期: 2026-07-27

## 背景（Context）

本地 coding agent 运行中会向用户提问：

- Claude Code：`AskUserQuestion` 工具（结构化单选/多选）
- OpenCode：原生 `question` 工具（SSE `question.asked`，需 `POST /question/{id}/reply` 解阻塞）
- Pi：RPC `extension_ui_request`（`select` / `confirm` / `input` / `editor`），需 `extension_ui_response` 解阻塞
- Codex：无等价结构化提问 hook（保持 passthrough / 自动审批）

本仓库此前对这类提问**未接管**：默认 `bypassPermissions` / `approval_policy=never` 绕过工具权限，
但模型主动反问时既无飞书 UI，也无法回填答案，表现为卡住、空答案或静默失败，**完全不可用**。

重点参考 [deepcoldy/botmux](https://github.com/deepcoldy/botmux) 的 ask-broker + 飞书 ask 卡片 +
CLI hook / OpenCode 插件链路（见其 `src/core/ask-*.ts`、`src/im/lark/ask-card.ts`、
`src/adapters/hook-installer.ts` 与 `docs/TEST-GUIDE-ask-hooks.md`）。

## 选项（Options）

1. **仅在系统提示里要求模型用文字提问** — 实现零成本，但不拦截原生工具，模型仍会调 `AskUserQuestion`/`question` 并阻塞。
2. **照搬 botmux 全量 hook 体系（全局 settings + 插件 + IPC）** — 成熟，但与本仓库「单进程 bridge、多 profile、stream-json/serve」架构叠床架屋。
3. **统一 ask broker + 飞书卡片；按 agent 选最短接管路径** — OpenCode 处理 SSE；Pi 处理 RPC
   `extension_ui_request`；Claude 用 profile 级 `--settings` PreToolUse hook + loopback IPC；
   Codex 文档声明不适用。

## 决策（Decision）

选 **3**。

- 新增 `internal/ask`：与 IM 解耦的 pending-ask 注册表（超时 / 单选即答 / 多选 toggle+submit / settle / freeform 文字作答）。
- 飞书侧发独立交互卡片（非流式 run 卡），回调同步回写终态。
- OpenCode：`question.asked` → `EventAskUser` → broker 出卡 → 用户作答 → `POST /question/{id}/reply`。
- Pi：`extension_ui_request`（select/confirm/input/editor）→ `EventAskUser` → 出卡 →
  `extension_ui_response` 写回 RPC stdin；input/editor 支持聊天文字 freeform。
- Claude：profile 目录 `claude-settings.json` 安装 `PreToolUse` matcher=`AskUserQuestion`，子命令
  `hook claude` 读 stdin、调 bridge loopback、stdout 写回 allow + answers directive。
- Codex：本阶段不接管结构化反问（无可靠 hook）；工具权限仍走既有 access 模式。

## 理由（Rationale）

- 与 botmux 的产品语义一致（「提问进飞书卡片，点选后 agent 继续」），但贴合本仓库事件抽象
  （`agent.Event`）与 CardKit 回调，避免引入 tmux/TUI 进程模型。
- OpenCode 我们已持有 SSE 总线与 directory 路由，插件二次转发是多余跳板。
- Pi RPC 已有官方 Extension UI 子协议，适配器内直接应答最短。
- Claude hook 必须可降级：daemon/env 不可达时 **passthrough 空 stdout**（与 botmux 一致），
  不把非 bridge 会话的提问答空。

## 后果（Consequences）

- 正面：Claude / OpenCode / Pi 反问在飞书可操作；run 期间 idle 看门狗在 pending ask 时暂停。
- 负面：Claude 依赖 hook 与本机 HTTP；用户本机 `claude` 若忽略 `--settings` 则退化为未接管。
- Codex 结构化反问仍不可用（与 botmux 一致，非本 ADR 缺口）。
- 工具级权限审批（Bash 是否执行）仍默认由 access mode 决定，不在本 ADR 范围做成审批卡。
- Pi 的 fire-and-forget UI（notify 等）仅作文本提示或忽略，不弹卡。
