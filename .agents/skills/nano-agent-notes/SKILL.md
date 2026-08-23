---
name: nano-agent-notes
description: Use when planning, implementing, reviewing, superseding, rejecting, or archiving any non-trivial nano-harness change; writes the required four-section Agent Note, keeps shipped decisions in present tense, separates durable ADR authority, audits overlapping notes, freezes archived records, and verifies that each material PR carries decision evidence.
---

# 维护 Nano Harness Agent Notes

Agent Note 记录一次非平凡改动的调查结论、决定、后果和实际验证，不记录逐步思维过程。每次 material change 在同一 PR 添加或更新 Note，不推迟补写。

## 先读规则

读取 [Agent Note 规则](../../notes/README.md)、[目录指令](../../notes/AGENTS.md)、[模板](../../notes/TEMPLATE.md)以及同主题现有 Note。再读当前代码、配置、测试、文档和 ADR，确定事实 owner。

## 判断是否必须写

以下任一项都是非平凡：可观察行为、错误/取消语义、并发或资源生命周期、架构依赖、公共/wire API、持久化格式、安全/权限、依赖、CI/CD、测试政策、submodule 更新，或需要 reviewer 理解长期理由的重构。

纯机械格式、拼写或无行为局部调整只有在 maintainer 明确认定后才可豁免。不要自行把“改动很小”等同机械。

## 选择生命周期

- `proposed/`：决定仍待评审或尚未完整落地。
- `implemented/`：代码、流程和验证已经落地；正文使用当前时态描述 shipped reality。
- `rejected/`：经过调查且明确不采纳；只在被否方案仍是一个可信、容易重犯的错误时保留。
- `archived/`：已被后来决定完整取代的冻结历史，不是当前权威。

实现同一 PR 中的 proposed Note 时，将其移至 `implemented/` 并把计划语言改成当前事实。不要留下已经完成的迁移 checklist；保留唯一理由、替代方案、后果、验证契约和已知证据缺口。

## 写四段证据

从模板创建 `YYYY-MM-DD-kebab-case.md`，保留状态与日期，并写：

- `Context`：当前问题、已有行为、直接证据、约束与非目标；区分生产 consumer 与测试/文档引用。
- `Decision`：明确选择、范围、owner、默认、失败/取消、兼容和禁止事项。插件改动写出 effect/cleanup 与真实 composition 路径。
- `Consequences`：收益、成本、失去的能力、风险、替代方案和重新评估条件。
- `Verification`：实际运行的精确命令、场景与结果，以及尚未获得的 live/platform 证据。100% coverage 只能证明语句执行，仍要写目标行为断言。

长期架构、协议、持久化、安全、依赖方向或发布承诺另写 `docs/decisions/` ADR。Agent Note 链接 ADR 并保留本次实施证据，不复制全文。

## 新 Note 同时审计重叠项

搜索相同机制、符号、配置、wire 字段和被否替代方案。对每个旧 active Note 分类：

- 完整取代：新 owner 吸收所有仍有价值的理由、替代、后果和验证后，旧 implemented Note 可完整移入 `archived/`；修复 active prose 的 inbound links。
- 部分取代：保留并双向链接，写清各自仍拥有的事实。
- 过时 proposed：转 rejected，并写诚实原因。
- 失去防错价值的 rejected：删除并修复 inbound links，不为数量保留。

年龄和字数只帮助发现，不是归档依据。归档后不得编辑正文、移动、删除或作为当前规则引用；不要进行 archive-wide 文案清理。

## 验证

```bash
make agent-notes
git diff --check
```

在 PR/CI 中提供正确的 `AGENT_NOTE_BASE_REF`，使门禁验证 material diff 携带 active Note 且 archived Note 未被修改。报告新增/更新/归档/删除的 Note、重叠判断、ADR 关系、实际验证和 borderline retention 决定。
