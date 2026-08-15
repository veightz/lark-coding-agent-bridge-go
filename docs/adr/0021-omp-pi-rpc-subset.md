# ADR-0021：Oh My Pi（omp）复用 Pi RPC 子集并隔离会话路径

- 状态：Accepted
- 日期：2026-08-09
- 关联：扩展 ADR-0002、ADR-0008、ADR-0019；与 `pi` agentKind 并列，不合并

## 背景（Context）

本机新增 Oh My Pi（CLI 二进制 `omp`，实测 v17.x），它是 Pi coding agent 的重度 fork：

1. 原生机器接口仍是 `--mode rpc`（stdin JSONL 命令、stdout 事件流），`get_state` /
   `get_available_models` / `set_model` / `prompt` / `abort` 与 Pi 同形；启动会有
   `ready`、`extension_ui_request`、`available_commands_update` 等噪声，现有
   “忽略非 response” 逻辑可兼容。**一轮 prompt 结束**：omp v17 发 `agent_end`
   （`isTerminal: true`），**不发** Pi 的 `agent_settled`；适配层映射
   `agent_settled`/`agent_end` → `EventDone`。**不要**把 `turn_end` 当 Done：
   多工具循环里会 mid-turn 出现 `tool → turn_end → turn_start → text → agent_end`，
   过早关 channel 会丢掉后续文本。
2. **会话恢复 CLI 与 Pi 不同**：omp **没有** `--session-id`；应使用
   `--resume <id|prefix|path>`（或 `--session-dir` / `PI_CODING_AGENT_DIR`）。
3. **默认配置树与 Pi 隔离**：omp 默认 `~/.omp/agent`（会话在
   `~/.omp/agent/sessions`），仍识别 env `PI_CODING_AGENT_DIR` /
   `PI_CODING_AGENT_SESSION_DIR`；不得默认扫 `~/.pi`。
4. **工具名表不同**：read-only 若照搬 Pi 的 `read,grep,find,ls` 会直接启动失败
   （`ls`/`find` 非法）。omp 合法只读子集含 `read` / `grep` / `glob` 等；另有内置
   `ask` 工具（不必装 `pi-ask-user` 也能反问），Extension UI 形状仍走
   `extension_ui_request` → 飞书 ask 卡。

若把 omp profile 误配成 `agentKind=pi` 或共用 PiAdapter 硬编码的 flag/路径，会
启动失败、扫错会话目录，或 read-only 无法生效。

## 选项（Options）

1. **不接入 omp** — 用户只能在终端用 omp，飞书侧无 profile。
2. **整份复制 Pi 适配器再改** — 事件翻译双份维护，易漂移。
3. **复用 Pi RPC 适配为协议子集，参数化差异** — 同一套 prompt/事件/UI 反问/abort；
   仅二进制名、ID/显示名、会话恢复 flag、配置/会话根、read-only `--tools`、
   用量标签与日志前缀不同。

## 决策（Decision）

选择 **3**：

1. 新增独立 `agentKind=omp`（不与 `pi` 合并、不做会话自动迁移）。
2. `NewAdapter(omp)` 返回参数化后的 Pi RPC 适配（同一实现类型或薄封装），启动
   argv 含 `--mode rpc`，续会话用 **`--resume`**，**永不**传 `--session-id`。
3. 会话发现与 `/usage` 默认扫描 `~/.omp/agent/sessions`（可用
   `PI_CODING_AGENT_DIR` / `PI_CODING_AGENT_SESSION_DIR` 覆盖），与 `~/.pi` 分离。
4. `read-only` 使用 omp 合法工具 allowlist：`--tools read,grep,glob`（可含只读类
   内置工具；不含 `bash`/`edit`/`write`）。`full`/`workspace` 保持 omp 默认工具集；
   不宣称 OS sandbox。
5. 能力对齐范围（与 ADR-0019 同级的最小集）：`/model` 列表与热切换、`/usage` 本机
   session 聚合、Extension UI / 内置 ask 反问飞书卡、`/sessions`+`/bind` 发现 omp
   磁盘会话、`abort` 优雅中断。
6. **Non-goals**（本 ADR 不实现）：plan-yolo/prewalk、browser-relay、`omp acp`、
   auth-broker、approval-mode 飞书审批卡、subagent 编排、security_scan 产品面等。

## 理由（Rationale）

- RPC 事件与命令与 Pi 共享，复制 translator 无收益；差异集中在启动参数与路径，
  参数化成本最低且单测可断言 argv/目录。
- 独立 kind 避免把已有 pi profile 的会话/配置与 omp 混写，也避免用户误以为
  `pi` 二进制能跑 omp 独有工具表。
- 验收以本机 `omp` 实测最小子集为准，不承诺与上游 pi 逐 flag 一致。

## 后果（Consequences）

- 飞书侧可配置 `--agent omp` / `agentKind: omp`，探测 `omp` 可执行文件。
- 同一台机器可同时跑 `pi` 与 `omp` profile，会话目录默认互不干扰。
- omp 工具表若上游再变，只需改 allowlist 常量与 ADR/测试；事件层通常不动。
- 完整多轮 prompt live 仍依赖模型鉴权/网络；无鉴权时以假二进制集成测证明接线。
