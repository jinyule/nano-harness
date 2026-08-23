# Agent Notes

Agent Note 记录一次非平凡变更的调查、决策、实施约束和验证证据。它比 ADR 更宽：并发修复、测试策略、CI 变更、重要重构和上游规则采纳都需要 Agent Note；只有纯机械格式、拼写或无行为的局部调整可豁免。

编写、审计、取代或归档 Note 时使用 [`nano-agent-notes`](../skills/nano-agent-notes/SKILL.md)；本文件仍是生命周期和强制范围的权威。

## 生命周期

- `proposed/`：仍在评审或尚未完全实施。
- `implemented/`：决定及其代码/流程已经落地，是当前权威。
- `rejected/`：已调查但明确不采纳，保留证据避免重复试错。
- `archived/`：被后续 Note 取代的历史记录。归档后冻结，不得编辑或作为当前规则引用。

文件名使用 `YYYY-MM-DD-kebab-case.md`。从 `TEMPLATE.md` 开始，四个二级标题必须存在。Note 描述当前变更的完整证据，不粘贴推理流水账，不代替代码文档或 ADR。

## 强制范围

以下任一项视为非平凡：可观察行为、错误/取消语义、并发/资源生命周期、架构依赖、公共或 wire API、持久化格式、安全/权限、依赖、CI/CD、测试政策、submodule 更新，或需要 reviewer 理解长期理由的重构。

PR 必须新增或更新 `proposed/`、`implemented/` 或 `rejected/` 下至少一个 Note。CI 根据 PR base 检查。真正机械的变更可由 maintainer 添加 `agent-note-exempt` 标签豁免；标签本身是评审决定，PR 描述仍须说明为何机械。

实施完成时把 Note 放入 `implemented/`。若一个决定长期约束架构、协议、安全或发布，还要同步写 ADR；Agent Note 记录本次工作的证据，ADR 记录长期决定。

## 验证

```bash
make agent-notes
```

本地没有 base ref 时只检查现有 Note 格式。CI 通过 `AGENT_NOTE_BASE_REF` 检查 PR 是否携带 Note，并拒绝修改已归档记录。
