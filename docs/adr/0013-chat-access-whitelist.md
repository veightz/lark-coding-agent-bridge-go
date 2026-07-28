# ADR-0013: 聊天访问白名单（对齐原版 access /invite）

- 状态: accepted
- 日期: 2026-07-28
- 对照: [lark-channel-bridge](https://github.com/zarazhangrui/lark-coding-agent-bridge) `src/policy/access.ts` / `owner.ts` / `/invite` `/remove`

## 背景（Context）

Go 复刻此前**未实现访问控制**：群里任意用户 @bot（或 `allowAutoReply`）即可驱动本地 agent。
用户在话题群拉人后，同事发消息也会被响应，存在安全与费用风险。

原版默认私有：

- **Owner**（飞书应用 owner open_id）：任意私聊/群聊可用，不受名单限制。
- **admins**：同 owner 的管理权 + 任意群可用。
- **allowedUsers**：可私聊 bot。
- **allowedChats**：名单内的群对**群内所有人**开放（owner/admin 不需要群在名单里）。
- 陌生人静默丢弃；未开放群里 @bot 时回一句 `/invite group` 提示。

## 决策（Decision）

移植原版语义（最小可用，不做 `/config` 卡片）：

1. `Profile.Access`：`allowedUsers` / `allowedChats` / `admins`（空列表 = 该维度无人，不是全开）。
2. 启动时拉取应用 owner（`application/v6/applications/{app_id}`），周期刷新（30min）。
3. `HandleMessage` 入口 `canUseDm` / `canUseGroup`；拒绝时除「未开放群 @bot」外一律静默。
4. 斜杠命令：`/invite user|admin|group|all group`、`/remove user|admin|group`（仅 owner/admin）。
5. 卡片回调（stop / ask）：操作者同样过 access 校验。
6. `/invite all group`：本期若无法列举 bot 所在群，则提示在目标群发 `/invite group`（后续可接 chat.list）。

## 后果（Consequences）

- 正面：默认仅 owner 可用；话题群里他人消息不再驱动 agent。
- 负面：依赖 application owner API；失败时 owner 视为未知，仅名单用户可用（与原版一致）。
- 显式建群 `/new chat` 不自动写入 `allowedChats`（owner 本身可在任意群用）。
