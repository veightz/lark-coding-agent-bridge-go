# ADR-0014: Codex 通过 app-server 托管反问与权限确认

- 状态: accepted
- 日期: 2026-07-31
- 关联: 扩展 [ADR-0008](./0008-agent-ask-feishu-cards.md) 与 [ADR-0011](./0011-opencode-permission-feishu-cards.md)

## 背景（Context）

ADR-0008 基于当时的 Codex CLI 能力，把 Codex 结构化反问和工具权限卡列为未实现。
现有 `CodexAdapter` 使用 `codex exec --json`：stdout 是单向 JSONL 事件，并强制
`approval_policy="never"`，因此无法接收或回填人机交互。

当前 Codex 官方 `app-server` 已提供双向 JSON-RPC 协议：

- `item/tool/requestUserInput`：结构化反问，客户端用 question id → answers map 回答；
- `item/commandExecution/requestApproval`：命令执行确认；
- `item/fileChange/requestApproval`：文件修改确认；
- `item/permissions/requestApproval`：模型主动申请额外文件系统/网络权限。

这些请求会阻塞 turn，直到客户端按原 JSON-RPC request id 回应，正好可接入现有
`EventAskUser` → ask broker → 飞书交互卡片链路。

## 选项（Options）

1. **继续使用 `codex exec --json`，用提示词要求文字提问并自动审批** — 无法托管原生
   `request_user_input`，也无法安全处理权限确认。
2. **解析 TUI 文本并写按键** — 协议脆弱，终端状态、版本和本地化都会改变输出。
3. **把 Codex 适配器迁到官方 `codex app-server --stdio`** — 使用稳定的 thread/turn
   生命周期和双向 server request；保持 bridge 的 `agent.Event` 抽象不变。

## 决策（Decision）

选 **3**。

- 每次 `Run` 启动一个 app-server 子进程，依次执行 `initialize` + `initialized`、
  `thread/start|resume`、`turn/start`；turn 结束后关闭子进程。线程连续性继续使用
  bridge 已持久化的 Codex `ThreadID`。
- 初始化协商 `experimentalApi=true`，因为 `requestUserInput` 属于实验协议；未知通知
  忽略，协议错误转成 `EventError`。
- `requestUserInput` 转为 `EventAskUser(Source=codex)`；选项 label 作为回填值，
  `isOther` 单题允许用户直接发聊天文字。
- 命令与文件审批转为 `EventAskUser(Source=codex-permission)`，按钮为
  `once / always / reject`，分别回填 `accept / acceptForSession / decline`；
  超时、run 停止或卡片失效一律 `decline`。
- `request_permissions` 展示申请原因、目录、文件系统与网络范围。允许一次/本会话允许
  回传所请求权限的完整子集及 `turn/session` scope；拒绝回传空权限集。
- `defaultAccess=full` 使用 `danger-full-access + approvalPolicy=never`，没有权限弹窗；
  `workspace/read-only` 使用对应 sandbox + `approvalPolicy=on-request`，由飞书接管所有
  app-server approval request。
- `Stop` 先发 `turn/interrupt`，随后按既有 grace 终止子进程；pending ask 由 bridge
  失效并安全拒绝。

## 理由（Rationale）

- app-server 是 Codex 富客户端使用的官方协议，具备 request id 相关性，不依赖 TUI 文本。
- 复用统一 ask broker 后，权限检查、CardKit 回调、超时 settle、idle 暂停都与其他
  agent 一致。
- access=full 保留现有默认体验；收紧 access 时不再把 `approval_policy=never` 偷偷覆盖
  用户的安全选择。

## 后果（Consequences）

- Codex 需要支持 `app-server`；不支持该子命令时 Run 会明确失败，不静默退回到无法托管
  交互的 `exec --json`。
- `requestUserInput` 是 Codex 实验协议，字段可能演进；适配器只依赖最小字段并以表驱动
  测试固定兼容边界。
- MCP elicitation 不是本 ADR 的模型反问；收到未支持的 server request 时会返回
  JSON-RPC method-not-supported 错误，避免 turn 永久阻塞。
