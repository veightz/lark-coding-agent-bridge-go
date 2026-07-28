# ADR-0010: 私聊按话题隔离 agent 会话（主时间线每条新开）

- 状态: accepted
- 日期: 2026-07-28

## 背景（Context）

私聊改为「以用户原消息为根、`reply_in_thread` 开话题」后（见实现层
`ReplyInThread`），主时间线每条用户消息在产品语义上是**一个独立话题**；
话题内再继续对话才是同一条线。

旧行为：`Message.Scope()` 在无私聊 `thread_id` 时等于整个 `chatID`，
`sessions.json` 整段私聊共用一个 agent session。结果：

1. 主时间线连续两条无关问题会 **resume 上一条会话**，上下文串台；
2. 卡片底部看不出两次运行是否同一 agent session；
3. 与「每条开话题」的 UI 模型不一致。

群聊与「群↔会话绑定」（ADR-0005/0007）语义不变。私聊任务自动建群
（ADR-0006）已由 ADR-0012 取消，与「话题回复已够用」一致。

## 选项（Options）

1. 保持 chat 级 session，仅 UI 开话题 — 实现简单，但上下文仍串。
2. **私聊 scope = `chatID:topicRoot`**（根消息 id）；主时间线每条新
   scope → 新 session；话题内 `root_id` 相同 → 续聊。
3. 每次 p2p 强制清空 `SessionID`，不改 scope — 话题内也无法续聊。

## 决策（Decision）

选 2：

1. **`TopicRootID()`**  
   优先 `RootID`，否则（已在话题但缺 root）`ThreadID`，否则本条
   `MessageID`（主时间线首条将成为话题根）。

2. **`Scope()`**  
   - `chat_type=p2p` → `chatID + ":" + TopicRootID()`  
   - 群聊保持：`chatID` 或 `chatID:threadID`（飞书话题群/话题内）。

3. **绑定**  
   `recordGroupBinding` 继续跳过 p2p；私聊话题 scope **不写入**
   `bindings.json` 的群复用表。`/bind` `/open` `/sessions` 仍以
   CLI 会话与**群**为主，不把 p2p 话题当成可 open 的群绑定。

4. **卡片**  
   结束行（耗时 / token）追加短 `SessionID` 或 Codex `ThreadID`
  （`🆔 <id 前 12 位>`），便于肉眼判断是否同一会话。

## 理由（Rationale）

- scope 与飞书话题根对齐，debounce 队列、session 存储、reaction、ask
  路由自然隔离，无需另建「强制 new session」旁路。
- 话题内续聊只依赖事件里的 `root_id`（与首条 `message_id` 一致），
  与 `reply_in_thread` 创建的话题模型一致。
- 不碰群绑定；自动建群另见 ADR-0012。

## 后果（Consequences）

- 历史 `sessions.json` 里以裸 `chatID` 为 key 的私聊条目不再被主时间线
  命中；旧上下文不会自动续上（符合新产品语义）。
- 若飞书事件在话题内偶发缺少 `root_id` 只给 `thread_id`，scope 会落到
  `omt_*`，与首条 `om_*` 不一致导致无法续聊——需日志观察；必要时再
  做 thread→root 映射表。
- `/status` 在私聊话题内显示当前 scope + agent 会话 id；主时间线新
  消息显示「无会话 / 将新建」。
- 卡片 footer 略变长；无 session 回传的 agent 则不显示 `🆔`。
