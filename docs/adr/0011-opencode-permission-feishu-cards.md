# ADR-0011: OpenCode 工具权限请求以飞书卡片接管

- 状态: accepted
- 日期: 2026-07-28
- 关联: 扩展 [ADR-0008](./0008-agent-ask-feishu-cards.md)（原 ADR 明确将工具级权限审批排除在外）

## 背景（Context）

OpenCode 在工具执行前会按项目 `permission` 配置发出 SSE：

- `permission.asked`（v1：`permission` + `patterns` + `metadata` + `always`）
- `permission.v2.asked`（v2：`action` + `resources` + `save`）

解阻塞需 `POST /permission/{requestID}/reply`，body：

```json
{ "reply": "once" | "always" | "reject" }
```

本仓库已接管 `question.asked`（ADR-0008），但**未消费 permission 事件**。结果是：当 opencode 配置为 `bash: ask` 等策略时，agent 在无 TUI 的 headless serve 模式下挂起，飞书侧无任何交互，用户只能停 run。

Claude/Codex 侧工具权限仍主要由 `defaultAccess`（`bypassPermissions` / `approval_policy=never`）静默放行，与 botmux 一致；本 ADR **仅覆盖 OpenCode**。

## 选项（Options）

1. **始终自动 `always`** — 实现成本最低，但抹平了 OpenCode 项目级 permission 策略，有安全回退风险。
2. **按 `defaultAccess` 分流**（full→auto always，其余弹卡）— 与 Claude 语义更齐，但用户已在 opencode.json 显式 `ask` 时会被静默绕过，不符合「权限询问」产品预期。
3. **全部 permission 事件弹飞书卡**（复用 ask broker）— 与 question 路径一致；超时/失效回 `reject` 安全解阻塞。

## 决策（Decision）

选 **3**，并按 bridge `permissions.defaultAccess` 分流：

- **`full`（默认，空值同 full）**：适配器在收到 `permission.asked` 后立即
  `POST /permission/{id}/reply` 且 `reply=always`，**不弹飞书卡**（对齐 Claude
  `bypassPermissions` / Codex `danger-full-access`；OpenCode 侧等价 YOLO / `"*": "allow"`）。
- **`workspace` / `read-only`**：弹出飞书权限卡，三选项
  `once` / `always` / `reject`；超时/取消 → `reject`。
- 适配器 `translate`：`permission.asked` / `permission.v2.asked` → `EventAskUser`，`Source=opencode-permission`。
- 卡片文案展示类型、目标 patterns/resources、常见 metadata 字段，以及 always/save 记忆提示。
- 人工确认路径：`Reply` → `POST /permission/{id}/reply?directory=…`。
- Bridge 对 permission 源回传 **option key**（非 label），避免中文标签污染 API enum。
- 卡片 chrome 使用「需要权限确认」标题（`ask.BuildCard` 按 source 区分）。
- auto-allow 的 HTTP 失败时降级弹卡，避免 agent 永久挂起。

## 理由（Rationale）

- 最短路径：不新增 broker 类型，复用 ADR-0008 的 Register / 卡片回调 / idle 看门狗暂停。
- OpenCode 已有稳定 HTTP 契约（v1 `/permission/{id}/reply`），与 question 对称。
- 超时拒绝比超时放行更安全。

## 后果（Consequences）

- 正面：OpenCode 权限询问可在飞书点选继续；不再静默挂起。
- 负面：项目级 `*: ask` 会带来较多卡片；用户可用 opencode 配置放宽或点「始终允许」沉淀规则。
- Claude / Codex / Pi 工具权限审批卡仍未做（与 ADR-0008 未实现列表一致，后续可另开 ADR）。
- 不改变 `permissions.defaultAccess` 对 Claude/Codex 的既有语义。
