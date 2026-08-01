# ADR-0018：Codex 与 OpenCode 面向用户能力取并集

- 状态：Accepted
- 日期：2026-08-01
- 关联：扩展 ADR-0007、ADR-0008、ADR-0011、ADR-0014、ADR-0016

## 背景

Bridge 已同时支持 Codex app-server 与 OpenCode serve，但两条适配链路的产品能力
并不完全一致：Codex 已接入模型选择和账户额度，OpenCode 原生支持的模型目录、
本地使用统计、自由文本反问没有暴露；同时 `permissions.defaultAccess` 在 OpenCode
侧只决定权限事件是否弹卡，没有形成与 Codex 对等的访问策略。

目标不是统一底层协议，而是让飞书侧获得两者能力的并集，并保留供应商特有数据。

## 决策

1. **权限语义按 profile 固化到 OpenCode server**：启动 `opencode serve` 时通过
   `OPENCODE_CONFIG_CONTENT` 注入权限覆盖，并与已有内容合并。
   - `full`：全部允许；已有 permission ask 仍由 auto-allow 兜底。
   - `workspace`：工作区内编辑允许；shell、网络请求需确认；外部目录拒绝。
   - `read-only`：编辑、shell、外部目录拒绝；网络请求需确认。
   这是 OpenCode 工具权限层的等价策略，不宣称提供 Codex OS sandbox 的同等隔离强度。
2. **模型选择通用化**：`ModelProvider` 接收当前工作目录。OpenCode 通过
   `GET /config/providers?directory=...` 获取当前项目可用模型与默认项；卡片不再硬编码
   Codex。OpenCode 模型引用保留 `provider/model[#variant]`，发送 prompt 时将 variant
   拆为独立字段。
3. **反问能力取并集**：OpenCode `question.custom` 映射为飞书文字自由回答；选项
   description 合并进问题文案。受现有 ask broker 限制，仅单题开启自由文本接管。
4. **用量保留不同语义**：`/usage` 的统一数据结构同时容纳额度窗口和本地活动统计。
   Codex 继续展示账号额度；OpenCode 通过官方 `opencode db ... --format json` 查询聚合
   会话数据，展示 session、token、cache、reasoning 与原生成本。任一适配器只填自己
   能可靠提供的字段。
5. **会话枚举覆盖全部项目**：OpenCode 先查询 `GET /project`，再对各 worktree 与
   sandbox directory 查询 `/session`，按 session id 去重并全局排序；单项目失败不影响
   其他项目，全部失败才返回错误。

## 后果

- `/model`、`/usage`、自由文本反问和跨项目 `/sessions` 对 Codex/OpenCode 都可用。
- OpenCode server 的 profile 权限优先于项目内 permission 配置；切换 profile access
  需要重启 Bridge，这与其他 profile 配置一致。
- OpenCode 数据库查询依赖其官方 `db --format json` 命令及 session 聚合列；协议变化
  时返回明确错误，不解析面向人的 `opencode stats` 表格。
- OpenCode 权限策略是工具层约束。第三方插件或工具若绕过 OpenCode permission 系统，
  仍不具备 Codex sandbox 的进程级保证。
