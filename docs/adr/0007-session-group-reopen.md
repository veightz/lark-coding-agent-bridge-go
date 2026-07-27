# ADR-0007: 群↔会话自动关联与私聊 /open 复用群

- 状态: accepted
- 日期: 2026-07-27

## 背景（Context）

OpenCode（及其他 agent）在群里跑完任务后，`sessions.json` 只有
`scope → sessionId` 正向映射；`bindings.json`（`sessionKey → chat`）
原先仅在用户显式 `/bind` 时写入。结果：

1. 私聊 `/sessions` 看不到哪些会话已经有对应群；
2. 用户想从私聊「给某个历史 session 再进群续聊」时，只能新建群或手动
   `/bind`，已有绑定群会被重复创建；
3. OpenCode 适配器未向 bridge 回传 `SessionID`（SSE 无 system 帧），
   群跑完后甚至 `sessions.json` 也可能空缺。

用户明确要求：私聊可查最近会话；对 session「重新建群」时，**已绑定过群
则复用**，只给跳转按钮，不要新建。

## 选项（Options）

1. 仅加强 `/bind` 文案，让用户自己建群再 bind — 步骤多，易重复建群。
2. 私聊 `/open <id>`：查绑定表，有群则跳转，无则建群并绑定。
3. 列表卡片按钮一键 open — 交互更好，但 CardKit 回调面更大，可后置。

## 决策（Decision）

选 2，并补齐关联落盘与 OpenCode session 回传：

1. **OpenCode 回传 SessionID**  
   `prompt_async` 成功后立刻 `EventSystem{SessionID}`（对齐 pi），
   使 `runBatch` 能写 `sessions.json`。

2. **群跑完自动写 bindings**  
   `runBatch` / `forwardToGroup` 在群 scope 拿到 session/thread id 后
   调用 `Bindings.SetIfAbsent`：仅当 sessionKey 空闲或已指向同一 scope
   时写入；**不抢**显式绑到别处的 session。`Binding` 增加
   `ChatType`（`p2p`/`group`）供复用判断。

3. **命令 `/open <序号或id前缀>`**（私聊主场景）  
   - `/sessions` 列表展示绑定标记，并提示 `/open`；  
   - 查 `bindings.json`，缺失时用 `sessions.json` 反查并回填；  
   - 绑定群且 `GetChat` 仍可达且非 p2p → **复用**，发「群已存在」跳转卡片；  
   - 否则 `CreateChat` → 写 sessions + bindings → 欢迎消息 → 新建跳转卡片；  
   - 旧群不可达时清绑定再新建。

4. **跳转卡片**  
   `sendGroupJumpCardEx(..., reused bool)` 区分文案；按钮仍为
   applink `openChatId`。

## 理由（Rationale）

- 自动关联与 `/open` 复用同一张 bindings 表，满足「一个 session 一个群」
  去重（延续 ADR-0005），并覆盖自动建群路径（ADR-0006）此前未写反向索引的空洞。
- `/open` 比改 `/bind` 语义更清晰：`/bind` 仍是「绑到**当前**聊天」；
  `/open` 是「确保该 session 有一个群并跳转」。

## 后果（Consequences）

- 历史仅有 `sessions.json`、无 bindings 的数据：`/open` 首次会反查并回填。
- 群被解散或 bot 被移出后 `/open` 会重建群（旧绑定清理）。
- 未实现把用户重新拉回已有群（Invite API）；若用户已退群，需自行点跳转
  或依赖飞书侧权限，后续可补 `chat.members` 邀请。
- 卡片按钮列表（选项 3）仍可作为后续体验增强，不阻塞本决策。
