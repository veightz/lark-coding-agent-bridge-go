# lark-coding-agent-bridge-go

[zarazhangrui/lark-coding-agent-bridge](https://github.com/zarazhangrui/lark-coding-agent-bridge)（npm 包名 `lark-channel-bridge`）的 **Go 语言复刻增强版**：把飞书 / Lark 消息桥接到本地 coding agent（**Claude Code / Codex CLI / pi / Oh My Pi(omp) / opencode / Grok / Kimi / Cursor**）。扫码绑定 PersonalAgent 应用后，即可在聊天里直接驱动本地 coding agent。

### 重点学习参考

实现与产品语义上，**优先对照** [deepcoldy/botmux](https://github.com/deepcoldy/botmux)（飞书遥控 AI 编程 CLI）。尤其是：

| 能力 | botmux 参考 | 本仓库落点 |
| --- | --- | --- |
| 模型反问 → 飞书卡片 | `ask-broker` / `ask-card` / hook-installer | `internal/ask` + ADR-0008 |
| Claude `AskUserQuestion` | PreToolUse hook + IPC | `hook claude` + profile `claude-settings.json` |
| OpenCode `question` | 插件转 hook | 适配器直接消费 SSE `question.asked` |
| OpenCode 工具权限 | （视实现） | SSE `permission.asked` → 飞书卡 → `POST /permission/{id}/reply`（ADR-0011） |
| Pi Extension UI | （botmux 走 TUI，本仓库用 RPC） | `extension_ui_request` → 卡片 → `extension_ui_response` |
| Grok `ask_user_question` | （ACP 客户端） | `grok agent stdio` reverse RPC `_x.ai/ask_user_question` → 卡片（ADR-0020） |
| Cursor `cursor/ask_question` | （ACP 客户端） | `cursor-agent acp` reverse RPC → 卡片（ADR-0022）；不把 Grok `agent` 当成 Cursor |
| 降级 | daemon 不可达 passthrough | 同语义（空 stdout / 不 reply / cancelled） |

原版起点仍是 [lark-channel-bridge](https://github.com/zarazhangrui/lark-coding-agent-bridge)。

## 功能

- **消息桥接**：私聊直接发消息，群里 `@bot`，消息转发给本地 agent。
- **多种 agent**：
  - `claude` — spawn-per-message stream-json（`--resume` 续会话）
  - `codex` — spawn-per-message `codex app-server --stdio`（双向 JSON-RPC，thread resume 续会话）
  - `pi` — **常驻 RPC 进程**（每 scope 一个 `pi --mode rpc`，原生 JSONL 协议，`abort` 优雅中断）
  - `omp` — **Oh My Pi**，常驻 RPC（复用 Pi 事件协议；`--resume` 续会话、默认 `~/.omp/agent`，ADR-0021）
  - `opencode` — **常驻 HTTP server**（`opencode serve`，REST + 按工作目录划分的 SSE 事件流，`abort` API 中断，`/global/health` 存活探测）
  - `grok` — **常驻 ACP 进程**（每 scope 一个 `grok agent stdio`，`session/prompt` + reverse ask，ADR-0020）
  - `kimi` — spawn-per-message stream-json
  - `cursor` — **常驻 ACP 进程**（每 scope 一个 `cursor-agent acp` 或已校验的 Cursor `agent acp`，`session/prompt` + `cursor/ask_question`，ADR-0022）
- **流式卡片**：回复实时渲染在 CardKit 2.0 流式卡片上（文本、思考、工具调用折叠面板、⏹ 终止按钮）。
- **会话保持**：每个私聊 / 群 / 话题各自维护独立会话；pi/omp/opencode 会话落盘，bridge 重启后自动续聊。
- **排队与合并**：短时间连续消息自动合并；运行期间发来的消息排队到下一轮；`/new`、`/cd`、`/stop` 可随时打断。
- **三层健壮性**：SDK 自动重连 + supervisor 存活探测强制重建（半死连接自愈）+ run 级 idle 看门狗（默认 10 分钟无事件自动终止并标注卡片）。
- **多工作区**：`/cd` 切换目录，`/ws` 保存 / 复用命名工作区。
- **图片和文件**：直接发给 bot，自动下载本地并附上路径（pi/omp 走 base64 内嵌，opencode 走 file part）。
- **模型反问卡片**（ADR-0008/0018/0020/0021/0022）：Claude `AskUserQuestion` / OpenCode `question` / Pi·omp `extension_ui_request` / Grok `ask_user_question`（ACP）/ Cursor `cursor/ask_question` 在飞书弹出交互卡片，点选或直接回复文字后 agent 继续。
- **权限模式**（ADR-0011/0014/0018/0019/0021）：Codex / OpenCode 的受限模式按原生协议执行权限策略并在需要时弹飞书确认卡；Pi `read-only` 只启用 `read/grep/find/ls`；omp `read-only` 启用 `read/grep/glob`（不得照搬 pi 的 find/ls）。
- **模型与用量**（ADR-0016/0018/0019/0021）：Codex、OpenCode、Pi、omp 均支持 `/model`；`/usage` 展示 Codex 账户额度，或 OpenCode / Pi / omp 本地 session/token/cache/cost 统计。
- **扫码向导**：首次运行终端渲染二维码创建 / 绑定 PersonalAgent 应用（复刻 registerApp 协议），也支持 `--app-id`。
- **多 profile**：独立凭证、会话、工作区与媒体缓存（`--profile` 必填）。
- **dashboard**：查看本机所有运行中的 bridge 实例及版本（区分发布版 / 开发分支构建）。
- **upgrade**：从源码仓库 `git pull --ff-only` + 重建 + 原子替换二进制。

## 与原版的差异

本复刻覆盖核心链路，以下原版功能**尚未实现**：后台守护进程服务管理、云文档评论、COT 过程消息、`/resume`、`/config` 交互卡片、`/doctor`、卡片回调签名、`/invite all group` 批量拉群、secrets 加密存储（App Secret 明文存于 `config.json`，文件权限 0600）。**访问控制**（owner + `/invite`/`/remove`）已实现（ADR-0013）。Codex 的结构化反问、命令/文件修改确认与额外权限申请已通过 app-server 接管（ADR-0014）。

设计与实现文档见 [`docs/design.html`](docs/design.html)（技术设计，含架构/时序/状态机图）与 [`docs/implementation.html`](docs/implementation.html)（关键结构与方法）。

## 构建

要求：Go >= 1.25。

```bash
mkdir -p bin
go build -o bin/lark-coding-agent-bridge-go ./cmd/lark-coding-agent-bridge-go
```

## 使用

运行前，本机需已安装并登录至少一个 agent CLI（`claude` / `codex` / `pi` / `omp` / `opencode` / `grok` / `kimi` / `cursor-agent`）。

### Pi 反问扩展（pi-ask-user）

使用 `pi` agent 时，**模型反问能力依赖社区扩展 `pi-ask-user`**：模型向你提问时，Pi 通过该扩展的 Extension UI 协议（`extension_ui_request`）把问题交给 Bridge，再由 Bridge 在飞书弹出选择/确认/输入卡片，你的回答会回填给模型继续执行。Bridge **不会自动安装**这个扩展，需要在运行 pi profile 之前手动装一次：

```bash
pi install npm:pi-ask-user
```

验证是否已安装（输出应包含 `npm:pi-ask-user`）：

```bash
pi list
```

- 未安装时，模型拿不到 `ask_user` 工具，反问会失效：模型无法弹出卡片提问，遇到需要确认的决策时可能自行猜测或行为异常。
- `permissions.defaultAccess=read-only` 的 pi profile 会通过工具 allowlist 禁用全部扩展工具，此时反问同样不可用——这是安全优先的有意取舍（ADR-0019）。
- 扩展装在本机 pi 全局配置（`~/.pi/agent/settings.json` 的 `packages` 列表），一台机器装一次即可，对所有 pi profile 生效。

### Oh My Pi（omp）

`omp` 与 `pi` 并列，**不要**把 omp profile 配成 `agentKind=pi`：

- 协议：同样是 `--mode rpc` + JSONL（事件翻译复用 Pi 路径，ADR-0021）。
- 续会话：`--resume <id>`（**没有** `--session-id`）。
- 会话目录：默认 `~/.omp/agent/sessions`（可用 `PI_CODING_AGENT_DIR` 覆盖），不与 `~/.pi` 混用。
- 反问：omp 内置 `ask` 工具，通常不必安装 `pi-ask-user`；Extension UI 仍回填飞书 ask 卡。
- `read-only`：`--tools read,grep,glob`（非法工具名会启动失败）。

### Cursor CLI

`cursor` 与 `grok` 并列，**不要**把 Grok 的 `agent` 二进制当成 Cursor（ADR-0022）：

- 协议：官方 ACP（`<cursor-agent|已校验 agent> acp`），常驻进程 + `session/new|load`。
- 探测：优先 `cursor-agent`；名为 `agent` 时必须通过 Cursor 身份校验。本机 Grok `~/.local/bin/agent` 会被拒绝。
- 反问：`cursor/ask_question` → 飞书 ask 卡；`cursor/create_plan` 本轮自动取消以免挂死。
- 权限：`session/request_permission` 自动放行（`--force` 是 print 路径的无人值守等价物，运行时不用 print）。
- 本轮不做 `/model`、`/usage`、Plan/Ask 斜杠命令、磁盘 `/sessions`+`/bind`。

```bash
# 启动（默认命令即 run；未保存应用凭证且未指定 --app-id 时进入扫码向导）
./bin/lark-coding-agent-bridge-go run --profile claude-dev --agent claude
./bin/lark-coding-agent-bridge-go run --profile pi --agent pi --app-id cli_xxx
./bin/lark-coding-agent-bridge-go run --profile omp --agent omp --app-id cli_xxx
./bin/lark-coding-agent-bridge-go run --profile oc --agent opencode --app-id cli_xxx
./bin/lark-coding-agent-bridge-go run --profile cursor --agent cursor --app-id cli_xxx

# 观测与运维
./bin/lark-coding-agent-bridge-go dashboard      # 运行中的实例、版本、心跳
./bin/lark-coding-agent-bridge-go upgrade        # 从源码仓库拉新并重建升级
./bin/lark-coding-agent-bridge-go upgrade --check
./bin/lark-coding-agent-bridge-go version
```

指定 `--app-id` 时会跳过扫码，并提示输入 App Secret。

常用参数：`--profile <name>`、`--agent claude|codex|pi|omp|opencode|grok|kimi|cursor`、`--workspace <path>`、`--app-id <id>`。

不同 profile 可以同时运行；同一 profile 由进程锁限制为单实例。重复启动会立即退出并提示当前占用该 profile 的 PID。

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
| `/model` | 用交互卡片切换当前会话模型（Codex / OpenCode / Pi / omp） |
| `/usage` | 查看账户额度或本地使用统计（Codex / OpenCode / Pi / omp） |
| `/help` | 帮助 |

私聊无需 @；群聊需要 `@bot`。

## 数据目录

| 路径 | 内容 |
| --- | --- |
| `~/.lark-coding-agent-bridge/config.json` | 根配置（profiles） |
| `~/.lark-coding-agent-bridge/registry/processes.json` | 进程注册表（dashboard） |
| `~/.lark-coding-agent-bridge/profiles/<name>/bridge.lock` | 同 profile 单实例进程锁（ADR-0017） |
| `~/.lark-coding-agent-bridge/profiles/<name>/sessions.json` | 会话状态 |
| `~/.lark-coding-agent-bridge/profiles/<name>/bindings.json` | 会话↔聊天绑定表（ADR-0005/0007；群跑完自动写入） |
| `~/.lark-coding-agent-bridge/profiles/<name>/workspaces.json` | 工作区绑定 |
| `~/.lark-coding-agent-bridge/profiles/<name>/media/` | 附件缓存 |
| `~/.lark-coding-agent-bridge/workspaces/<name>/default/` | 默认工作目录 |

设置 `LARK_CODING_BRIDGE_HOME=/path/to/state` 可迁移全部本地状态。

## Agent 启动前置命令（按机器配置）

无法直连外网的机器，可以为每个本机 profile 单独配置 agent CLI 的启动前置命令。
它只在 Bridge 启动 agent 的子 shell 中生效，不会修改 Bridge 主进程、当前终端或全局
代理环境。未配置时仍直接启动 CLI。

配置文件默认是 `~/.lark-coding-agent-bridge/config.json`；如果设置过
`LARK_CODING_BRIDGE_HOME`，则是 `$LARK_CODING_BRIDGE_HOME/config.json`。找到启动
Bridge 时 `--profile <name>` 对应的 `profiles.<name>`，在其中加入 `agent` 字段。

### 使用脚本、function 或普通命令

推荐把代理初始化写在可执行脚本中，或者在 `commandPrefix` 里显式 `source` 所需文件：

```json
{
  "profiles": {
    "my-codex": {
      "agentKind": "codex",
      "agent": {
        "commandPrefix": "source ~/.proxy-env && proxy_on",
        "shell": "/bin/zsh"
      }
    }
  }
}
```

也可以直接导出仅供 agent 使用的代理变量：

```json
"agent": {
  "commandPrefix": "export HTTPS_PROXY=http://127.0.0.1:7890 HTTP_PROXY=http://127.0.0.1:7890"
}
```

### 使用 shell alias

默认 shell 是 `/bin/sh`，默认参数是 `["-c"]`，不会自动加载 `.zshrc` 或 `.bashrc`。
如果 `proxy_on` 只是 `.zshrc` 中定义的 alias，需要显式选择 zsh 交互模式：

```json
"agent": {
  "commandPrefix": "proxy_on",
  "shell": "/bin/zsh",
  "shellArgs": ["-ic"]
}
```

修改配置后重启对应 Bridge 进程即可生效。

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `agent.commandPrefix` | 空 | 启动 agent 前执行的 shell 命令；为空时不启动 shell |
| `agent.shell` | `/bin/sh` | 执行前置命令的 shell |
| `agent.shellArgs` | `["-c"]` | shell 参数；alias 场景可使用 `["-ic"]` |

注意事项：

- 前置命令返回非零时 agent 不会启动。
- 前置命令成功后，shell 会用 `exec` 替换为真实 agent CLI。
- agent binary 和参数通过位置参数传递，不会被 shell 二次解析。
- 配置作用于该 profile 所选 agent 的 CLI 启动路径；Codex/OpenCode 包括正常对话、`/model` 和 `/usage`，Pi 包括正常对话与 `/model`（Pi `/usage` 直接读取本地 session JSONL，不启动 CLI）。
- 该设置同样支持 Claude、Pi、OpenCode、Grok 和 Kimi。
- 交互式 shell 会加载用户配置，可能产生额外输出或副作用，因此优先使用可执行 wrapper
  或显式 `source`。
- 它不会为 Bridge 自身连接飞书提供代理；如果飞书连接也需要代理，应单独设置 Bridge
  进程环境。

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

profile 配置中的 `permissions.defaultAccess` 控制 agent 工具权限（`full` 默认 / `workspace` / `read-only`）；`preferences.idleTimeoutMinutes` 控制 run 级 idle 看门狗（默认 10，负值关闭）。OpenCode 使用原生 permission 规则；Pi `read-only` 使用工具 allowlist。两者都不等同于 Codex 的进程级 OS sandbox，Pi `workspace` 也不能阻止 shell 访问工作区外路径。

## 开发

```
go test ./...                    # 单元 + 进程级 adapter 测试
LARK_LIVE_TEST=1 go test ./internal/agent -run Live -v   # 真实 CLI 冒烟（消耗少量额度）
go vet ./...
```

架构：`cmd/` 入口；`internal/onboard` 扫码注册；`internal/agent` 多 agent 适配器 + ACP client（Grok 主路径）；`internal/lark` OpenAPI 封装；`internal/card` 状态机与流式卡片；`internal/bridge` 消息路由与命令；`internal/supervisor` 连接监督；`internal/registry` 进程注册表；`internal/upgrade` 自升级；`internal/buildinfo` 版本信息。

## License

MIT（与原作一致）
