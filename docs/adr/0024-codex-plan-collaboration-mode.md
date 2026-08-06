# ADR-0024：Codex Plan 协作模式

- 状态：Accepted
- 日期：2026-08-07
- 编号说明：原并行分支写作 ADR-0021；合入 main 时 0021 已被 omp 占用，顺延为本编号。
- 关联：扩展 ADR-0014、ADR-0016、ADR-0018

## 背景（Context）

Bridge 已通过 Codex app-server 接管 thread、turn、反问与权限确认，但每轮只传模型、
sandbox 和 approval policy，没有传 Codex 的 collaboration mode。用户从飞书发起复杂
编码任务时只能直接进入执行，无法先使用 Codex 原生 Plan 模式梳理需求、检查仓库并与
用户确认方案。

Codex app-server 的实验协议提供 `collaborationMode/list`，并允许在 `turn/start` 中传
`collaborationMode`。模式对象除 `plan|default` 外还包含模型、推理强度和内置开发指令
语义；这些参数可能随本机 Codex CLI 版本变化。

## 选项（Options）

1. 在用户 prompt 前拼接“先规划、不要修改代码”——实现简单，但不是 Codex 原生模式，
   无法稳定继承其工具约束、反问策略和后续协议演进。
2. 在 profile 中固定 Plan 模式——适合专用 bot，但同一会话无法在规划和执行之间切换。
3. 按 scope 持久化协作模式，并在每轮通过 app-server 原生协议解析、传入预设——与 Codex
   桌面端语义一致，且不会写死模型或推理强度。

## 决策（Decision）

选择 **3**：

1. 新增 `/mode plan` 与 `/mode default`。模式按聊天 scope 写入 `Session.CollaborationMode`；
   切换时停止当前 scope 的运行，从下一条普通消息起生效。`/new` 重置会话时一并恢复默认。
2. 通过可选 `CollaborationModeProvider` 暴露 agent 支持的模式。首版只有 Codex 实现，其他
   agent 对 `/mode` 返回明确的“不支持”，不模拟不可靠的兼容行为。
3. Codex Plan turn 在 thread start/resume 后调用 `collaborationMode/list`，选择 CLI 当前公布
   的 `plan` 预设；预设未指定模型时使用实际 thread model，再将完整
   `collaborationMode` 传入 `turn/start`。`developer_instructions=null` 表示使用 Codex 内置
   Plan 指令。
4. `default` 不传覆盖字段，保持 app-server 默认行为。CLI 不支持 Plan 预设时本轮明确失败并
   提示升级 Codex CLI，不静默退化为提示词模拟。

## 理由（Rationale）

- 原生协作模式同时约束模型行为、推理强度和开发指令，语义比 prompt 约定可靠。
- 会话级切换让用户可以先规划，再在同一 Codex thread 中切回 Default 执行，保留完整上下文。
- 运行时读取 CLI 预设可跟随协议升级，同时不覆盖用户通过 `/model` 选择的当前 thread 模型。
- provider 接口把能力判断留在 adapter，bridge 不依赖 agent ID 硬编码。

## 后果（Consequences）

- Codex profile 可在飞书用 `/mode plan` 进入只规划的协作轮次，用 `/mode default` 恢复执行。
- `sessions.json` 新增可选 `collaborationMode` 字段；旧状态文件向后兼容。
- Plan 模式依赖支持 `collaborationMode/list` 的 Codex app-server 实验协议；版本过旧时会收到
  可操作的升级提示。
- Claude、Pi、OpenCode 暂不提供 `/mode`；它们已有的 access/read-only 权限语义不等同于
  Codex Plan 协作模式。
