# ADR-0023：统一 Bridge 与 lark-cli 的默认飞书主体

- 状态：Accepted
- 日期：2026-08-05
- 关联：扩展 ADR-0015、ADR-0017
- 编号说明：原并行分支写作 ADR-0020；合入 main 时 0020 已被 Grok ACP 占用，顺延为本编号。

## 背景（Context）

Bridge 以 `config.json` 中 profile 的 `appId/appSecret` 建立飞书长连接，但 agent
子进程此前只收到一个 profile 私有的 `LARKSUITE_CLI_CONFIG_DIR`。该目录没有从 Bridge
凭据初始化，`lark-cli` 可能未配置、保留旧应用，或默认选择 user 身份。结果是同一轮
对话里 Bridge bot 与 agent 调用 `lark-cli` 的应用/身份主体不一致，历史上已多次出现。

官方 `lark-cli >= 1.0.43` 支持 `config bind --source lark-channel` 与
`LARK_CHANNEL_CONFIG`，并以独立的 `lark-channel` workspace 隔离 Agent 配置。Bridge
已经按 profile 隔离 `LARKSUITE_CLI_CONFIG_DIR`，可以在启动边界完成确定性的绑定。

## 选项（Options）

1. 继续仅注入私有配置目录 — 无迁移成本，但无法保证目录里的 App 与身份策略正确。
2. 让用户为每个 profile 手工执行 `lark-cli config init/bind` — 可用，但形成两份需要
   人工同步的配置，App Secret 轮换后仍会漂移。
3. 以 Bridge profile 为唯一凭据源，启动时按需同步 lark-cli — 默认强一致；需要处理
   CLI 版本、绑定失败和显式分离配置。

## 决策（Decision）

选择 **3**：

1. `profiles.<name>.app` 是该 profile 唯一人工维护的飞书应用配置。Bridge 启动时由
   `internal/larkcli` 检查该 profile 的 lark-cli workspace；源指纹或有效绑定不一致时，
   自动执行 `lark-cli config bind --source lark-channel --identity bot-only`。
2. 默认身份锁定为 `bot-only`，同时保证“同一个 App”和“同一种 bot 主体语义”。
3. 生成 `lark-cli-source.json` 作为官方 lark-channel bind schema 的投影。投影通过
   file provider + JSON Pointer 回读 Bridge 根配置，不复制 App Secret 明文。
4. 绑定成功后校验 `app_id` 与 identity，再写非秘密同步指纹；两者均匹配时不重复绑定，
   避免 Bridge 重启清除 lark-cli 已有的用户 OAuth 记录。
5. 显式例外：`larkCli.sharedApp=false` 完全退出自动绑定；
   `larkCli.identity=user-default` 仍共享同一 App，但允许 lark-cli 默认代表已授权用户。
6. 未安装 lark-cli 时 Bridge 正常启动；已安装但版本低于 1.0.43、同步失败或结果主体
   不匹配时启动失败，禁止静默降级到未知主体。

## 理由（Rationale）

- 默认配置即安全行为，不依赖 agent 是否记得先检查 `whoami`。
- 使用官方 bind 协议而非自行猜测 lark-cli 私有存储格式，可复用其 keychain、严格身份
  策略与 workspace 隔离。
- “Bridge 配置为源 + 派生绑定”只保留一份人工事实；同步指纹让 Secret/租户/策略变化
  可被检测，同时避免无变化时破坏 OAuth 状态。
- 默认 bot-only 不允许 agent 无提示地代表用户访问个人日历、邮箱或云空间；扩大到
  user-default 必须出现在 profile 的显式配置中。

## 后果（Consequences）

- 现有 profile 第一次以新版 Bridge 启动时会自动创建/更新其 lark-cli 绑定。
- 已安装 lark-cli 的机器需要版本不低于 1.0.43；升级命令为 `lark-cli update`。
- 修改 Bridge App Secret 会触发重新绑定；若该 profile 显式使用 user-default，可能需要
  重新进行用户 OAuth 授权。
- `sharedApp=false` 后一致性由操作者负责，Bridge 仍保留 profile 级配置目录隔离与
  `LARK_CHANNEL=1` 上下文，但不再提供主体一致性保证。
