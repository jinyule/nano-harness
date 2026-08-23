# AGENTS.md — Repository Skills

- 每个 skill 聚焦一类可重复任务；frontmatter `description` 必须写清触发场景和硬边界，不能只是目录名释义。
- `SKILL.md` 保留工作流与 guardrail；长例子和搜索语料放 `references/`，确定性重复动作优先调用仓库脚本。
- 链接使用相对路径并在最终检查中验证。不要复制 `third_party/deepseek-harness` 的仓库专属命令、术语或许可证不明代码。
- skill 必须引用本仓当前权威文件和真实命令。规则变化先改 owner，再同步 skill，不让 skill 成为第二份漂移的规范。
- 新增或实质修改 skill 是非平凡流程变更，需要 Agent Note，并运行 skill validator、`make agent-notes` 与 `git diff --check`。
