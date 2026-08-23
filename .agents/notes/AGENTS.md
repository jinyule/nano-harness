# AGENTS.md — Agent Notes

- 每个非平凡改动在同一 PR 添加或更新 Agent Note；不要把 Note 推迟到后续 PR。
- 使用 `TEMPLATE.md` 的四段结构，写结论、证据、约束和实际命令，不写逐步思维过程。
- 完成的决定放 `implemented/`；仍待评审放 `proposed/`；有证据的否决放 `rejected/`。
- `archived/` 冻结：只允许把完整旧 Note 移入，不得编辑其正文，也不得把它链接为当前权威。
- 长期架构/协议/安全/发布决定同时维护 `docs/decisions/` ADR；避免在两处复制全文，用链接明确权威。
