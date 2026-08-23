# 将上游 Skills 适配为 Go 项目可执行工作流

- Status: implemented
- Date: 2026-08-24

## Context

初始基线分析覆盖了 deepseek-harness 的架构、工程规则和 CI/CD，但没有把 `.agents/skills/` 中的任务路由、精确 scope、语义证据、文案完整命题和确定性脚本经验转化为本仓可调用能力。上游固定提交包含 11 个 skill；其中部分绑定 VitePress、双语 pairing、浏览器 GUI 或 GitHub stacked PR，而插件、review、pre-push、Agent Note、简化和文案方法可直接改善当前 Go 项目。任何适配本身属于非平凡流程变更，必须遵守本仓 Agent Note 规则。

## Decision

仓库在 `.agents/skills/` 维护七个项目专属 skill：`nano-plugin-development`、`nano-code-review`、`nano-pre-push-checks`、`nano-agent-notes`、`nano-find-simplifications`、`nano-doc-standards` 和 `nano-prose-standard`。每个 skill 以本仓 `AGENTS.md` 和 `docs/` 为权威，使用精确触发 description、明确 scope/禁止事项/完成证据，并通过 `agents/openai.yaml` 暴露调用元数据。插件、逐文件 100% coverage 和每个非平凡改动写 Agent Note 是所有相关 skill 的阻断条件。新增 `scripts/change-scope.sh`，只读取已验证引用或 unborn worktree，不猜测或 fetch base；其测试覆盖首个提交前和普通 committed/staged/unstaged/untracked 范围。文档站、双语翻译、浏览器 GIF 和 stacked PR skill 在真实产品或远端协作边界出现前不落地。

## Consequences

高频任务拥有可直接调用且能随仓库演进的工作流，review 和实现不再依赖 agent 记住分散规则；插件生命周期、100% coverage 与 Agent Note 在实现、审查和推送三个阶段重复成为硬证据。维护者需要在权威规则变化时同步检查相关 skill，并为实质 skill 变更继续写 Note。暂缓项不会提前引入文档投影、翻译 hash、浏览器工具或远端合并权限；对应边界建立后必须重新分析上游当前版本，而不是直接启用旧命令。

## Verification

`scripts/change-scope_test.sh` 验证 unborn 和普通分支范围并通过；`scripts/change-scope.sh` 在当前 unborn 仓库正确列出 staged、unstaged 和 untracked 文件。skill-creator 的 `quick_validate.py` 通过 Ruby 标准库 YAML adapter 对七个 skill 全部返回 `Skill is valid!`；仓库自己的 `make skills` 进一步校验目录名、frontmatter、`agents/openai.yaml` 元数据和相对链接。`make check` 通过，其中 race tests、架构、submodule、Agent Note、workflow tools、golangci-lint、真实 binary smoke 均为绿色，所有产品源文件和函数 coverage 为 100.0%。Actionlint v1.7.12、Markdown 相对链接、占位符、行尾空白和 `git diff --check` 也通过。元数据生成器因宿主 Python 缺少 PyYAML 未执行，其静态输出按照已读 `openai.yaml` schema 写入并由 `make skills` 校验。
