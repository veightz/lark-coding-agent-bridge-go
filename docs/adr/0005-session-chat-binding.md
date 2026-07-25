# ADR-0005: 会话 ↔ 聊天绑定（CLI 会话接入飞书）

- 状态: accepted
- 日期: 2026-07-25

## 背景（Context）

很多 agent 会话是在命令行里直接跑的，并非从飞书发起。用户希望：
(1) 在聊天里把**已有的 CLI 会话**绑定到当前会话/群，之后在该聊天里
直接续聊该会话；(2) 一个会话已被绑定后，在别处再次绑定时不要重复
创建，而是告知它绑在哪个聊天，避免一个会话对应多个群。

## 选项（Options）

1. 只按 cwd 匹配自动续接 — 隐式、不可控，cwd 相同的不同会话无法区分。
2. 显式绑定：发现（列出磁盘会话）+ 绑定表（sessionKey ↔ scope 双向去重）。
3. 每次绑定都新建群 — 打扰用户；绑定到当前聊天即可，建群留给飞书原生操作。

## 决策（Decision）

选 2：

- **发现**：`/sessions` 列出当前 profile agent 的最近磁盘会话
  （claude `~/.claude/projects/*/*.jsonl`、pi `~/.pi/agent/sessions/*/*.jsonl`、
  codex `~/.codex/sessions/**/*.jsonl`、opencode 经 serve `GET /session`），
  含 id 前缀、cwd、首条用户消息摘要、更新时间。
- **绑定**：`/bind <id前缀>` 把当前 scope 与该会话绑定——写入
  sessions.json（scope → sessionId/threadId + **该会话自己的 cwd**）与
  bindings.json（sessionKey=agent:id → scope）。runBatch 优先使用
  会话记录的 cwd。
- **去重**：sessionKey 已绑定到其他 scope 时拒绝并告知目标聊天
  （名称查 im/v1/chats，p2p 显示 chat_id）；`/bind <id> --force`
  显式换绑；`/unbind` 解除当前聊天的绑定。

## 理由（Rationale）

- 显式 > 隐式：会话选择是用户意图，绑定关系落盘可审计。
- sessionKey 唯一约束天然防止"一个会话多个群"。

## 后果（Consequences）

- 文件扫描是 best-effort 解析（首行 header + 首条用户消息），
  格式演进时按 agent 独立修发现器；均有表驱动单测。
- claude 会话目录名有损编码（连字符歧义），cwd 以文件内 `cwd` 字段为准。
- 换绑不迁移历史消息，仅改变后续 prompt 的 resume 目标。
