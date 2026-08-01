# ADR-0016：会话级模型选择卡片

- 状态：Accepted
- 日期：2026-07-31

## 背景

Bridge 过去只能被动记录 agent 返回的模型。Codex CLI 会自动选用默认模型，
用户若想切换，需要离开飞书会话到本机 CLI 操作，且不同账号、不同 Codex 版本
能使用的模型列表会变化。

## 决策

1. 新增 `/model` 斜杠命令。第一阶段仅接入 Codex。
2. adapter 通过可选 `ModelProvider` 接口暴露当前账号可用模型。Codex 使用
   app-server `model/list` 动态查询，不在 Bridge 中硬编码型号。
3. Bridge 使用 CardKit 2.0 卡片展示模型，并通过统一
   `card.action.trigger` 回调处理选择。
4. 模型选择按现有 scope 持久化到 `sessions.json` 的 `Session.Model`；
   下一次 run 通过 `RunOptions.Model` 传给 adapter。Codex 在
   `thread/start` / `thread/resume` 时应用该值。
5. 卡片选项绑定一次性随机 nonce，服务端只接受该卡片实际展示过的模型；
   nonce 15 分钟过期，成功选择后立即消费。
6. 切换模型时停止同 scope 的在途 run。run 收尾保存 session 时保留并发写入的
   新模型，避免旧 run 覆盖用户刚完成的选择。

## 后果

- 模型列表跟随本机 Codex CLI 和登录账号自动更新。
- 模型设置与会话隔离；群聊按群/话题生效，私聊按话题生效。
- Bridge 重启后已经选择的模型仍保留，但未点击的旧卡片会失效，用户需重新发送
  `/model`。
- 其他 agent 可在实现 `ModelProvider` 后复用相同交互，无需修改命令协议。
