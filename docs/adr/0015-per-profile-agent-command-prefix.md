# ADR-0015: profile 级 agent 启动前置命令

- 状态: accepted
- 日期: 2026-07-31

## 背景（Context）

部分部署机器不能直接访问外网，运行 Codex 等 agent CLI 前需要启用该机器特有的代理。
代理不应注入 bridge 主进程或全局 shell，也不适合写成所有机器共享的固定逻辑。直接
使用 `exec.Command` 又不会加载用户的 shell alias/function。

## 选项（Options）

1. **要求用户全局设置代理环境变量** — 影响 bridge、lark-cli 及同一 shell 下的其他
   程序，作用域过大。
2. **只提供固定 wrapper 可执行文件路径** — 安全清晰，但不能直接表达
   `source ...`、环境变量导出或已有的 shell 函数。
3. **在本机 profile 配置一段 command prefix** — 仅在启动 agent 的短生命周期 shell
   中执行，成功后 `exec` 原 agent CLI。

## 决策（Decision）

选 **3**。profile 新增：

```json
"agent": {
  "commandPrefix": "source ~/.proxy-env && proxy_on",
  "shell": "/bin/zsh",
  "shellArgs": ["-c"]
}
```

- 未配置 `commandPrefix` 时继续直接 `exec`，不增加 shell 层。
- 配置后执行 `<prefix>`；返回非零则停止启动；成功后执行
  `exec "$@"`，由 shell 替换为原 agent 进程。
- CLI binary 与 argv 通过 shell 位置参数传入，不拼接或重新解析 agent 参数。
- `shell` 默认 `/bin/sh`，`shellArgs` 默认 `["-c"]`。依赖 `.zshrc` / `.bashrc`
  alias 时可显式配置相应 shell 和 `["-ic"]`；更推荐可执行脚本或在 prefix 中显式
  `source`，避免交互式 shell 的额外输出与副作用。
- 配置应用于 profile 所选 agent 的所有子进程，包括 Codex `/usage` 查询；常驻型
  agent 在其 server/RPC 进程启动时应用。

## 后果（Consequences）

- 每台机器可在自己的 `config.json` 中独立配置，不需要修改仓库或全局代理。
- prefix 是本机受信任配置，可执行任意 shell 命令；配置文件继续保持 0600。
- prefix 只影响 agent 子进程及其后代，不修改 bridge 自身环境，也不会帮助 bridge
  连接飞书；若飞书连接本身也需代理，应单独配置 bridge 进程环境。
