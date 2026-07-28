# 群里免 @ 自动回复

默认情况下，机器人在群里只响应 @ 它的消息。开启自动回复后，群聊里所有消息机器人都会看到并响应。

## 配置步骤

### 1. 飞书开放平台开启权限

应用 → 权限管理 → 勾选 **「获取群聊中所有消息」**（`im:message:read_all`）。

> 普通消息权限只能读取 @ 了机器人的消息；不加这个权限 autoReply 不起作用。

### 2. profile 中开启

编辑 profile 配置（`~/.lark-coding-agent-bridge/profiles/<profile>/config.json`），添加：

```json
"preferences": {
  "allowAutoReply": true
}
```

### 效果

- 群里所有消息都会进入 bot 处理流程，无需 @
- `@all` 单独不算 @，但在 autoReply 模式下也会正常响应
- 私聊不受影响，始终自动回复

### 关闭

删掉 `allowAutoReply` 或将值改为 `false`，重启后恢复默认行为（群里需 @ 才响应）。
