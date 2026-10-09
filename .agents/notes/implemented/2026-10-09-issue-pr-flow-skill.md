# Issue 关联 PR 的协作规则与 nano-issue-pr-flow skill

- Status: implemented
- Date: 2026-10-09

## Context

维护者 2026-10-08 要求，此后的工作都走同一流程：先开 issue，PR 关联 issue，修好并合并 PR，再关闭 issue（#22）。在此之前，这条要求只存在于 agent 的个人记忆里，仓库中没有权威文字。#18、#19 没有关联 issue；#21 通过 `Closes #20` 关联了 issue，但合并时固定 head、合并后检查 main CI、补结案评论、清理本地这些步骤都是临时操作。[上游 skill 适配](2026-08-24-adapt-upstream-skills.md)曾把 stacked PR 等远端协作 skill 推迟到“真实远端协作边界出现”之后；现在仓库已经在 GitHub 上用 issue 和 PR 协作，这个边界已经出现。

## Decision

规则写在 [CI/CD 的 Issue 与 PR 关联](../../../docs/ci-cd.md#issue-与-pr-关联)，这是唯一的权威位置：

- 缺陷、功能和流程变更都先按模板开 issue；
- PR 正文第一行用 `Closes #N` 关联；
- 满足 `All checks passed` 为绿、review 已解决、维护者同意三个条件后，用 `--match-head-commit` 固定已验证的 head 做 squash 合并；
- 合并后确认 issue 已关闭，补一条结案评论，观察 main 的 CI；main 变红时开新 issue 修复。

Dependabot 更新和维护者确认的纯机械变更不需要 issue。

`AGENTS.md` 的 CI/CD 一节只链接这条规则；PR 模板的第一行提示用 `Closes #N` 关联。新增仓库 skill [nano-issue-pr-flow](../../skills/nano-issue-pr-flow/SKILL.md) 作为执行入口，按顺序串联 issue、分支、PR、CI 诊断、合并、结案和清理的真实命令。提交前门禁、审查和 Agent Note 分别继续由 nano-pre-push-checks、nano-code-review 和 nano-agent-notes 负责。`AGENTS.md` 与 `README.md` 的 skill 列表同步加入这个 skill，项目 skill 由 7 个变为 8 个。

分支保护、CI workflow 和合并权限都不改，也不引入 stacked PR 或自动化 bot。

## Consequences

有了固定的执行顺序，agent 按 skill 走流程时，不会再漏掉 issue 关联、head 固定、结案评论和合并后的 main CI 检查。规则以后变化时，先改 `docs/ci-cd.md`，再同步这个 skill。代价是每个非平凡变更多一次开 issue 的操作。合并仍然需要维护者许可，skill 不授予合并权限。

## Verification

本变更自身就按这个流程处理：#22 → PR。下列检查均已执行并通过：

- `make skills`：8 个 skill 的 frontmatter、`agents/openai.yaml` 和相对链接；
- `AGENT_NOTE_BASE_REF=origin/main make agent-notes`；
- `git diff --check`；
- 对改动文件中相对链接和锚点的检查。

skill 中的命令已经在 #19、#21 的实际处理过程中执行过，包括 `gh pr merge --squash --match-head-commit`、`gh issue comment`、`gh run watch --exit-status`。
