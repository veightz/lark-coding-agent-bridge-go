# ADR-0001: Go 复刻 lark-channel-bridge 的范围与接入方式

- 状态: accepted
- 日期: 2026-07-24

## 背景（Context）

原版 lark-channel-bridge（TS/Node，约 2 万行）功能面很大：多 profile、守护进程、
访问控制、云文档评论、COT 消息、secrets 加密等。Go 复刻需要在可控工作量内
先把核心价值跑通，并保留后续补齐的空间。

## 选项（Options）

1. 全量功能对齐 — 体验一致，但工作量巨大、周期长。
2. 核心链路 MVP，架构上为后续功能留位 — 先跑通主干。
3. 只做 demo 级验证 — 无法日常使用。

## 决策（Decision）

选 2：核心链路 MVP（消息桥接、流式卡片、会话保持、排队合并、基础斜杠命令、
图片文件下载），飞书应用接入复刻原版 registerApp 扫码协议（同时支持 --app-id）。

## 理由（Rationale）

- 扫码体验是原版的关键亮点，且协议可从官方 node-sdk 完整复刻
  （POST accounts.feishu.cn/oauth/v1/app/registration，begin → QR → poll）。
- 守护进程、访问控制等属于运营层，可在主干稳定后增量补。

## 后果（Consequences）

- 未实现清单固化在 README「与原版的差异」，新贡献者不要误报为 bug。
- App Secret 暂明文存储（0600），secrets 加密是已知待办。
