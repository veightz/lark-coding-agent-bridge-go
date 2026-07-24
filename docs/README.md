# docs/ — 设计文档体系

本仓库采用**设计驱动开发**：设计理念、关键决策、实现结构全部留在仓库里，
保证任何 agent / 任何时间接手都能重建完整上下文（而不是依赖某个 agent 的记忆）。

## 阅读顺序

1. [`../AGENTS.md`](../AGENTS.md) — 入口：工作模式、构建命令、仓库地图
2. [`design.html`](design.html) — 技术设计：总体架构、四种 agent 协议时序、
   流式卡片机制、三层健壮性、dashboard / upgrade（含 SVG 图，浏览器打开）
3. [`implementation.html`](implementation.html) — 实现层：关键结构体、接口、
   方法的签名级说明（对照源码阅读）
4. [`adr/`](adr/) — 架构决策记录：每个重要决策的背景、选项、结论与后果

## ADR 约定

- 文件名：`NNNN-kebab-case-title.md`，编号递增，不改旧编号。
- 已定稿的 ADR 不修改；推翻旧决策时写新 ADR，开头标注 `Supersedes ADR-NNNN`，
  并在旧 ADR 顶部加一行 `> 已被 ADR-NNNN 取代`。
- 模板见 [adr/0000-template.md](adr/0000-template.md)。

## 维护规则

- 行为级改动必须同步更新 design.html / implementation.html 对应章节。
- 架构级改动必须先有（或同 PR 附带）ADR。
- 文档与代码不一致时，以代码为准并尽快修文档——在 AGENTS.md 的收尾检查里兜底。
