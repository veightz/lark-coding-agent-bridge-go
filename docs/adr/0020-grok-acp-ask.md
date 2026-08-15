# ADR-0020：Grok 走 ACP 并接管 ask_user_question

- 状态：Accepted
- 日期：2026-08-07
- 关联：扩展 ADR-0002、ADR-0008；Grok 最小 stream-json 适配升级为本决策

## 背景（Context）

Bridge 已有 `GrokAdapter` 最小实现：`grok -p --output-format streaming-json`，
仅翻译 `thought` / `text` / `end`。本机实测：

1. Headless stream-json 下模型可发出 `ask_user_question`（`kind: ask_user`），
   但工具失败：`Failed to reach the client for user question … ext_method … channel closed`。
2. Grok PreToolUse hook 只支持 allow/deny，**不能**像 Claude 那样用 `updatedInput`
   注入答案。
3. `grok agent stdio`（ACP）会把反问以 reverse RPC 发给客户端：
   方法 `_x.ai/ask_user_question`，参数含 `sessionId`、`toolCallId`、`questions[]`、`mode`。

因此仅靠 spawn-per-message 的 headless 协议无法实现飞书反问卡。

## 选项（Options）

1. **继续 stream-json + 系统提示禁止 ask** — 实现成本低，但模型仍会调用工具并失败，体验差。
2. **PreToolUse hook 模拟 Claude** — Grok hook 无 `updatedInput` 回填，走不通。
3. **Grok 改用 ACP（`grok agent stdio`），接管 `_x.ai/ask_user_question`** — 与 ask 官方通道一致，可复用 `acp.go` 与飞书 ask broker。

## 决策（Decision）

选 **3**。

1. **协议**：Grok profile 使用 `grok agent stdio`（JSON-RPC ACP）。按 chat scope 复用常驻进程；`cwd` 变化或进程死亡时重建。
2. **会话**：`session/new` 取得 `sessionId`；续聊优先 `session/load`，失败再 `session/new`。Bridge 仍把 `sessionId` 写入 profile sessions。
3. **流式事件**：`session/update` → 既有 `translateACPUpdate`（text / thought / tool_call）。
4. **反问**：收到 `_x.ai/ask_user_question`（兼容 `x.ai/ask_user_question`）→ `EventAskUser`（`Source=grok`，`Freeform=true`）→ 飞书卡；用户作答后 respond：
   - 成功：`{"outcome":"accepted","answers":{ "<question>": "label" | ["labels"] }}`（value 为 `StringOrVec`）
   - 超时/取消/失效：`{"outcome":"cancelled"}`
5. **权限**：`full` → 启动 `--always-approve` 或 session `_meta.yoloMode=true`；`session/request_permission` 仍 auto-allow。`workspace` / `read-only` 尽量映射到非 yolo + 工具限制，**不宣称** OS sandbox。
6. **不**安装 Claude PreToolUse hook，不写 `claude-settings.json`。

## 理由（Rationale）

- ask 的官方客户端通道就是 ACP ext_method；headless 没有 client 通道。
- 仓库已有 ACP client 骨架与 ask broker；产品语义对齐 Claude/OpenCode/Pi。
- 常驻进程避免每轮 initialize 的开销，并支持 session 复用。

## 后果（Consequences）

- 正面：Grok 反问可在飞书点选/文字回填；工具与思考流进入 run 卡。
- 负面：依赖 Grok ACP 扩展方法命名与应答 schema；版本变更时需更新 format 层单测。
- 本轮不做：Grok `/model`、`/usage`、工具权限审批卡、磁盘 session 扫描绑定。
- stream-json 翻译器可作为兼容测试保留，**运行时不再作为主路径**。
