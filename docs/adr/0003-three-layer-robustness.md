# ADR-0003: 三层健壮性模型（SDK 重连 / supervisor 探测重建 / run 级看门狗）

- 状态: accepted
- 日期: 2026-07-25

## 背景（Context）

原版使用中经常出现"消息跑着跑着断联"，需要人肉 /reconnect。断联分两类：
干净断开（SDK 能感知并重连）与半死连接（TCP 看似活着但事件不再到达，
SDK 心跳窗口内察觉不到）。另外 agent 子进程也可能假死（无输出但不退出）。

## 选项（Options）

1. 只依赖 SDK 自动重连 — 半死连接无法自愈。
2. 应用层定期重启 WS（定时器）— 无差别重启，误伤健康连接。
3. 分层：SDK 重连 + REST 存活探测强制重建 + run 级 idle 看门狗。

## 决策（Decision）

选 3。supervisor 每 60s 调轻量 REST（bot info，10s 超时）；连续失败 3 次判定
半死 → client.Close() 并完整重建（含握手 15s 超时）；状态机
connecting/connected/reconnecting/down 可观测。run 级看门狗：agent 超过
preferences.idleTimeoutMinutes（默认 10，负值关闭）无事件 → run.Stop() +
卡片标注"⏱ 长时间无响应，已自动终止"。

## 理由（Rationale）

- REST 探测证明"凭证+网络+API 可达"，是比 TCP 心跳更强的事实来源；
  连续 3 次失败才重建，避免抖动误杀。
- 看门狗把"假死的 agent"转化为明确的卡片终态，用户可感知、可重发。

## 后果（Consequences）

- 探测每 60s 一次 REST 调用，代价可忽略。
- Status 快照已预留（State/LastEventAt/Restarts），dashboard 后续可展示
  连接维度信息。
- /stop 统一走各 agent 原生中断（pi abort / opencode abort / SIGTERM→SIGKILL），
  与看门狗共用同一路径。

## 补充（2026-07-25）：opencode 双通道读取

针对 opencode SSE 偶发丢帧的社区反馈，适配器再加两道保险：

1. **SSE 帧看门狗**：目录级流记录最近帧时间（heartbeat 也算），
   75s 无帧即判定半死，主动 cancel 重连（原仅在网络错误时重连）。
2. **轮询回补**：每个活跃 run 每 3s 拉一次 `GET /session/{id}/message`，
   以服务端消息列表为准对账——assistant 消息 completed 而 SSE 未收到
   `session.idle` 时，把丢失的文本后缀补发后再发终态事件。
   对账基准是 dispatch 时累积的 delivered 文本（见 reconcilePoll 单测）。
   SSE 仍是快路径（毫秒级流式），轮询只兜底（3s 粒度）。
