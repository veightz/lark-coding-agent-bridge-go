# ADR-0019：对齐 Pi 的模型、用量与访问能力

- 状态：Accepted
- 日期：2026-08-01
- 关联：扩展 ADR-0002、ADR-0008、ADR-0016、ADR-0018

## 背景（Context）

Bridge 已通过 Pi 原生 RPC 支持常驻会话、流式事件、图片、优雅中断和
`extension_ui_request` 反问，但飞书侧仍有三处明显缺口：

1. Pi RPC 原生提供 `get_available_models` / `set_model`，Bridge 却未实现
   `ModelProvider`，因此 `/model` 不可用；已有 `RunOptions.Model` 也只在进程首次启动
   时生效，复用常驻进程后无法真正切换模型。
2. Pi 原生会话保存完整 token、cache 与 cost，Bridge 却未实现 `UsageProvider`，
   因此 `/usage` 不可用。
3. `permissions.defaultAccess=read-only` 未传给 Pi，仍会保留 `bash` / `edit` /
   `write` 等可变更工具。

本机 Pi 0.83.0 的 RPC 和 session v3 格式均提供所需结构化字段，无需解析面向人的
终端输出，也无需增加第三方依赖。

## 选项（Options）

1. 继续只提供基础对话 — 改动最少，但同一套飞书命令在 Pi profile 上缺失，且模型
   设置存在“卡片显示已切换、实际进程未切换”的风险。
2. 通过 Pi RPC 与 session JSONL 对齐核心能力 — 使用官方机器协议获取模型并热切换，
   按 Pi 自身统计口径聚合本地会话用量，以工具 allowlist 实现只读模式。
3. 编写 Pi 扩展复刻 Codex 权限审批与 sandbox — 可做更细粒度控制，但 shell 无法靠
   扩展获得进程级隔离，复杂度与安全收益不匹配。

## 决策（Decision）

选择 **2**：

1. `PiAdapter` 实现 `ModelProvider`。以短生命周期 `pi --mode rpc --no-session` 发送
   `get_state` 和 `get_available_models`，模型 ID 统一保存为 `provider/model`；每次
   prompt 前比较常驻进程当前模型，必要时发送 `set_model`，保证持久化选择立即生效。
2. `PiAdapter` 实现 `UsageProvider`。扫描 Pi session JSONL，复用 Pi
   `get_session_stats` 的计费口径：累计 assistant、toolResult、compaction 与
   branch_summary 的 usage，展示本地 session/message/token/cache/cost 总量。
3. Pi 的 `read-only` profile 启动时使用 `--tools read,grep,find,ls`，移除内置
   `bash` / `edit` / `write` 及扩展工具。`full` 与 `workspace` 保持 Pi 原生工具集；
   `workspace` 仅表达产品策略，不宣称具备 Codex workspace-write 的 OS sandbox。
4. 延续现有 Extension UI 语义：对话型 UI 走飞书 ask 卡，`notify` 进入运行卡文本；
   `setStatus` / `setWidget` / `setTitle` 等瞬时 TUI 装饰不单独映射为飞书卡片。

## 理由（Rationale）

- RPC 是 ADR-0002 已选定的 Pi 官方机器接口，模型查询和切换均有请求响应关联，避免
  解析 `--list-models` 文本。
- Pi 是多 provider agent，不存在可靠的统一账户额度；本地 session 聚合与 OpenCode
  的 `/usage` 语义一致，也能忠实保留 Pi 原生成本。
- 工具 allowlist 能可靠消除 Pi 内置写入与 shell 工具，但不能把 Node 进程变成
  sandbox；明确边界比提供虚假的隔离保证更安全。

## 后果（Consequences）

- Pi profile 可复用统一 `/model` 和 `/usage` 命令，模型切换对已存在的常驻会话生效。
- `/usage` 是本机 Pi session 的累计活动，不是任一模型供应商的账户余额或额度窗口。
- `read-only` 会禁用所有扩展工具，即使某个扩展工具本身只读；这是安全优先的有意取舍。
- `workspace` 仍无法阻止 `bash` 访问工作区外路径；需要强隔离时应在 Pi 进程外增加容器
  或系统 sandbox，并另写 ADR。
