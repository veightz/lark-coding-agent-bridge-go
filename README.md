# lark-coding-agent-bridge-go

[zarazhangrui/lark-coding-agent-bridge](https://github.com/zarazhangrui/lark-coding-agent-bridge)（npm 包名 `lark-channel-bridge`）的 **Go 语言复刻增强版**：把飞书 / Lark 消息桥接到本地 coding agent（**Claude Code / Codex CLI / pi / opencode**）。扫码绑定 PersonalAgent 应用后，即可在聊天里直接驱动本地 coding agent。

### 重点学习参考

实现与产品语义上，**优先对照** [deepcoldy/botmux](https://github.com/deepcoldy/botmux)（飞书遥控 AI 编程 CLI）。尤其是：

| 能力 | botmux 参考 | 本仓库落点 |
| --- | --- | --- |
| 模型反问 → 飞书卡片 | `ask-broker` / `ask-card` / hook-installer | `internal/ask` + ADR-0008 |
| Claude `AskUserQuestion` | PreToolUse hook + IPC | `hook claude` + profile `claude-settings.json` |
| OpenCode `question` | 插件转 hook | 适配器直接消费 SSE `question.asked` |
| OpenCode 工具权限 | （视实现） | SSE `permission.asked` → 飞书卡 → `POST /permission/{id}/reply`（ADR-0011） |
| Pi Extension UI | （botmux 走 TUI，本仓库用 RPC） | `extension_ui_request` → 卡片 → `extension_ui_response` |
| 降级 | daemon 不可达 passthrough | 同语义（空 stdout / 不 reply / cancelled） |

原版起点仍是 [lark-channel-bridge](https://github.com/zarazhangrui/lark-coding-agent-bridge)。

## 功能

- **消息桥接**：私聊直接发消息，群里 `@bot`，消息转发给本地 agent。
- **四种 agent**：
  - `claude` — spawn-per-message stream-json（`--resume` 续会话）
  - `codex` — spawn-per-message `codex exec --json`（`resume` 续会话）
  - `pi` — **常驻 RPC 进程**（每 scope 一个 `pi --mode rpc`，原生 JSONL 协议，`abort` 优雅中断）
  - `opencode` — **常驻 HTTP server**（`opencode serve`，REST + 按工作目录划分的 SSE 事件流，`abort` API 中断，`/global/health` 存活探测）
- **流式卡片**：回复实时渲染在 CardKit 2.0 流式卡片上（文本、思考、工具调用折叠面板、⏹ 终止按钮）。
- **会话保持**：每个私聊 / 群 / 话题各自维护独立会话；pi/opencode 会话落盘，bridge 重启后自动续聊。
- **排队与合并**：短时间连续消息自动合并；运行期间发来的消息排队到下一轮；`/new`、`/cd`、`/stop` 可随时打断。
- **三层健壮性**：SDK 自动重连 + supervisor 存活探测强制重建（半死连接自愈）+ run 级 idle 看门狗（默认 10 分钟无事件自动终止并标注卡片）。
- **多工作区**：`/cd` 切换目录，`/ws` 保存 / 复用命名工作区。
- **图片和文件**：直接发给 bot，自动下载本地并附上路径（pi 走 base64 内嵌，opencode 走 file part）。
- **模型反问卡片**（ADR-0008）：Claude `AskUserQuestion` / OpenCode `question` / Pi `extension_ui_request` 在飞书弹出交互卡片，点选（或文字 freeform）后 agent 继续（对标 botmux ask 链路）。
- **OpenCode 权限**（ADR-0011）：默认 `full` 自动 `always` 放行；`workspace`/`read-only` 时 `permission.asked` 弹「允许一次 / 始终允许 / 拒绝」卡。
- **扫码向导**：首次运行终端渲染二维码创建 / 绑定 PersonalAgent 应用（复刻 registerApp 协议），也支持 `--app-id`。
- **多 profile**：独立凭证、会话、工作区与媒体缓存（`--profile` 必填）。
- **dashboard**：查看本机所有运行中的 bridge 实例及版本（区分发布版 / 开发分支构建）。
- **upgrade**：从源码仓库 `git pull --ff-only` + 重建 + 原子替换二进制。

## 与原版的差异

本复刻覆盖核心链路，以下原版功能**尚未实现**：后台守护进程服务管理、云文档评论、COT 过程消息、`/resume`、`/config` 交互卡片、`/doctor`、卡片回调签名、`/invite all group` 批量拉群、secrets 加密存储（App Secret 明文存于 `config.json`，文件权限 0600）。**访问控制**（owner + `/invite`/`/remove`）已实现（ADR-0013）。Codex 无结构化反问 hook，不接管 AskUserQuestion 类交互（与 botmux 一致）。

设计与实现文档见 [`docs/design.html`](docs/design.html)（技术设计，含架构/时序/状态机图）与 [`docs/implementation.html`](docs/implementation.html)（关键结构与方法）。

## 构建

```
go build -o bin/lark-coding-agent-bridge ./cmd/lark-coding-agent-bridge
```

要求：Go >= 1.22；本机已安装并登录至少一个 agent CLI（`claude` / `codex` / `pi` / `opencode`）。

## 使用

```
# 启动（默认命令即 run；首次进入扫码向导）
./bin/lark-coding-agent-bridge run --profile claude-dev --agent claude
./bin/lark-coding-agent-bridge run --profile pi --agent pi --app-id cli_xxx
./bin/lark-coding-agent-bridge run --profile oc --agent opencode --app-id cli_xxx

# 观测与运维
./bin/lark-coding-agent-bridge dashboard      # 运行中的实例、版本、心跳
./bin/lark-coding-agent-bridge upgrade        # 从源码仓库拉新并重建升级
./bin/lark-coding-agent-bridge upgrade --check
./bin/lark-coding-agent-bridge version
```

常用参数：`--profile <name>`、`--agent claude|codex|pi|opencode`、`--workspace <path>`、`--app-id <id>`。

## 聊天内命令

| 命令 | 作用 |
| --- | --- |
| `/new`, `/reset` | 清空当前会话（常驻型 agent 同时重置其进程/会话） |
| `/cd <path>` | 切换工作目录（会话重置） |
| `/ws list` / `save <name>` / `use <name>` / `remove <name>` | 命名工作区管理 |
| `/sessions` | 列出命令行里已有的 agent 会话（含绑定群标记） |
| `/bind <序号或id前缀> [--force]` | 把当前聊天绑定到该会话（去重，已绑定会提示所在聊天） |
| `/open <序号或id前缀>` | 为该会话复用已有群或新建群，私聊下发跳转按钮（已绑定则不新建） |
| `/unbind` | 解除当前聊天的会话绑定 |
| `/stop` | 停止当前运行（同卡片上的 ⏹ 按钮） |
| `/status` | 查看 profile、agent、工作目录、会话状态 |
| `/help` | 帮助 |

私聊无需 @；群聊需要 `@bot`。

## 数据目录

| 路径 | 内容 |
| --- | --- |
| `~/.lark-coding-agent-bridge/config.json` | 根配置（profiles） |
| `~/.lark-coding-agent-bridge/registry/processes.json` | 进程注册表（dashboard） |
| `~/.lark-coding-agent-bridge/profiles/<name>/sessions.json` | 会话状态 |
| `~/.lark-coding-agent-bridge/profiles/<name>/bindings.json` | 会话↔聊天绑定表（ADR-0005/0007；群跑完自动写入） |
| `~/.lark-coding-agent-bridge/profiles/<name>/workspaces.json` | 工作区绑定 |
| `~/.lark-coding-agent-bridge/profiles/<name>/media/` | 附件缓存 |
| `~/.lark-coding-agent-bridge/workspaces/<name>/default/` | 默认工作目录 |

设置 `LARK_CODING_BRIDGE_HOME=/path/to/state` 可迁移全部本地状态。

## 聊天访问控制（ADR-0013）

默认**私有**：只有飞书应用 owner 能用 bot。其他人消息静默忽略。

| 名单 | 作用 | 命令 |
|------|------|------|
| owner | 应用创建者，任意私聊/群 | 自动解析 |
| `access.allowedUsers` | 可私聊 | `/invite user @某人` |
| `access.admins` | 私聊 + 任意群 + 管名单 | `/invite admin @某人` |
| `access.allowedChats` | 该群内**所有人**可用 | 群里 `/invite group` |

配置写在 `config.json` 对应 profile 的 `access` 字段；也可用 `/remove …` 移除。

## 权限模式与看门狗

profile 配置中的 `permissions.defaultAccess` 控制 agent 工具权限（`full` 默认 / `workspace` / `read-only`）；`preferences.idleTimeoutMinutes` 控制 run 级 idle 看门狗（默认 10，负值关闭）。

## 开发

```
go test ./...                    # 单元 + 进程级 adapter 测试
LARK_LIVE_TEST=1 go test ./internal/agent -run Live -v   # 真实 CLI 冒烟（消耗少量额度）
go vet ./...
```

架构：`cmd/` 入口；`internal/onboard` 扫码注册；`internal/agent` 四种 agent 适配器 + ACP client（预留）；`internal/lark` OpenAPI 封装；`internal/card` 状态机与流式卡片；`internal/bridge` 消息路由与命令；`internal/supervisor` 连接监督；`internal/registry` 进程注册表；`internal/upgrade` 自升级；`internal/buildinfo` 版本信息。

## License

MIT（与原作一致）

