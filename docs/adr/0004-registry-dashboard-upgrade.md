# ADR-0004: 本地进程注册表 + dashboard + 源码自升级

- 状态: accepted
- 日期: 2026-07-25

## 背景（Context）

owner 同时跑多个实例（正式版 + 开发分支），原版进程管理"太抽象、无法观测"：
不知道哪些实例在跑、各自跑的是哪个版本的代码。还需要低成本的自升级路径。

## 选项（Options）

1. 依赖 OS 服务管理器（launchd/systemd）— 原版路线，观测性差，且锁死部署形态。
2. 进程注册表文件 + dashboard 子命令 + upgrade 子命令。
3. 内嵌 HTTP 管理端口 — 信息最全，但引入端口占用与安全面，YAGNI。

## 决策（Decision）

选 2。每个 bridge 进程注册到
~/.lark-coding-agent-bridge/registry/processes.json（pid/profile/agent/binary/
workspace/version/startedAt/heartbeatAt），15s 心跳，退出注销，读取时按
PID 活性清僵尸；version 来自 debug.ReadBuildInfo（vcs.revision/time/modified），
天然区分发布构建与 dirty 开发构建。upgrade = git fetch → 比对 → pull --ff-only
→ go build → 自检 → os.Rename 原子替换。

## 理由（Rationale）

- 注册表无锁设计（单进程只写自己条目），足够支撑本机多实例观测。
- ff-only + 临时文件构建 + 自检 + 原子替换，任何一步失败都不留残局；
  运行中的实例不受影响，dashboard 用 ⚠︎ 提示版本不一致。

## 后果（Consequences）

- 版本比对依赖 git 提交历史；无 commit 的仓库 upgrade 会报错提示。
- 升级后需手动重启实例生效（刻意不自动杀进程，避免打断进行中的会话）。
