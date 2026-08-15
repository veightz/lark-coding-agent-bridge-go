# ADR-0022：Cursor CLI 作为一等 agent（ACP 主路径，禁止误绑 Grok `agent`）

- 状态：Accepted
- 日期：2026-08-15
- 关联：扩展 ADR-0002、ADR-0008、ADR-0020；与 `grok` 并列，不共用二进制探测

## 背景（Context）

Cursor 官方 CLI 的机器接口有两条：

1. **Print**：`cursor-agent -p --output-format stream-json`（NDJSON：`system/init`、
   `assistant`、`tool_call` started/completed、`result`）。`--force` / `--yolo`
   可无人值守放行工具权限；`--resume` / `--continue` 续会话。print 模式通常
   抑制 thinking，也**没有**客户端通道应答 `cursor/ask_question`。
2. **ACP**：`agent acp` / `cursor-agent acp`（JSON-RPC over stdio）。官方 custom-client
   路径：`initialize` → `authenticate(cursor_login)` 或预登录 → `session/new|load` →
   `session/prompt`。阻塞反向 RPC：`session/request_permission`、`cursor/ask_question`、
   `cursor/create_plan`。通知类：`cursor/update_todos` / `cursor/task` /
   `cursor/generate_image`（本轮不产品化）。

官方安装物常见名为 `agent`。本机 `/Users/veightz/.local/bin/agent` 是 **Grok**
（`~/.grok/bin/agent` → `grok-*-macos-aarch64`）。若 `LookPath("agent")` 不加校验，
`agentKind=cursor` 会误绑 Grok。

## 选项（Options）

1. **只做 print + `--force`** — 实现接近 Claude，无法把 `cursor/ask_question`
   变成飞书卡；阻塞提问只能靠 print 侧跳过。
2. **官方 ACP（复用 `acp.go`）** — 权限可 auto-allow，反问可走 `EventAskUser`，
   续聊走 `session/load`；需把 Cursor 的 `agent` 与 Grok 的 `agent` 分开。
3. **双路径运行时自动切换** — 探测 ACP 失败再降 print，状态机复杂，本轮不需要。

## 决策（Decision）

选 **2**，print 翻译器与 argv 作为纯函数保留（单测与文档），**运行时主路径是 ACP**。

1. **kind**：独立 `agentKind=cursor`（`--agent cursor` / onboard）。不与 `grok` 合并。
2. **二进制**：优先 `cursor-agent`；其次名为 `agent` 但必须通过
   `LookLikeCursorCLI` 校验（解析 symlink 后路径含 `/.grok/` 立即拒绝；`--version` /
   `--help` 像 Grok 则拒绝；文案含 cursor 才接受）。**禁止**裸 `LookPath("agent")`。
3. **协议**：每 chat scope 常驻 `<binary> acp`。`session/new` 取 `sessionId`；
   续聊优先 `session/load`，失败再 `session/new`。`session/update` 复用
   `translateACPUpdate`。`session/request_permission` 沿用 `autoAllowPermission`
   （优先 `allow_always`）。`full` 与其它 access 本轮都不做飞书权限卡。
4. **反问**：`cursor/ask_question` → `EventAskUser`（`Source=cursor`）→ 飞书卡。
   官方结果是**嵌套**信封（扁平 `{"outcome":"answered"}` 会被真实 Cursor 拒掉）：
   `{"outcome":{"outcome":"answered","answers":[{questionId,selectedOptionIds}]}}`
   或 `{"outcome":{"outcome":"cancelled"}}`。其它阻塞扩展（含 `cursor/create_plan`）
   立即嵌套 `cancelled`，避免默认 agent 回合挂死。
5. **鉴权**：initialize 若宣告 `cursor_login` 则尝试 `authenticate`；已登录 /
   `CURSOR_API_KEY` 预鉴权即可。本桥不负责安装或登录 Cursor CLI。
6. **Non-goals**：`/model`、`/usage`、Cloud Agent、MCP 安装审批、Plan/Ask 斜杠命令
   与 `create_plan` 审批卡、磁盘 `/sessions`+`/bind`、`update_todos`/`task`/
   `generate_image` 一等卡片。

## 理由（Rationale）

- ACP 是 Cursor 官方 custom-client 通道；权限与 ask 都是阻塞 RPC，print 无法收尾。
- 仓库已有 `acp.go` 与 ask broker（Grok 同款），产品语义对齐 ADR-0008/0020。
- 二进制误绑是产品 bug：本机 Grok `agent` 必须永远不能被当成 Cursor。

## 后果（Consequences）

- 飞书可配置 `--agent cursor` / `agentKind: cursor`；探测只认 `cursor-agent` 或
  已校验的 Cursor `agent`。
- 常驻 ACP 适配器实现 `SessionResetter`（`/new` / `/cd` 杀进程）。
- Cursor CLI 未安装或未登录时，live 测试诚实 skip；接线由假二进制 Run + 表驱动
  翻译证明。
- print `stream-json` 翻译器保留，便于对照官方 NDJSON，**不是**运行时主路径。
