---
name: nano-issue-pr-flow
description: Use when taking a nano-harness defect, feature, or process change through GitHub, from opening the issue to closing it; covers creating the issue from the repository templates, a branch from fresh main, a PR that says Closes #N, watching and diagnosing CI, squash-merging only the verified head with maintainer approval, confirming the issue closed, following the main CI, and local cleanup. Does not replace pre-push checks, review, or Agent Note rules.
---

# Nano Harness Issue 与 PR 流程

规则的权威位置是 [CI/CD 的 Issue 与 PR 关联](../../../docs/ci-cd.md#issue-与-pr-关联) 和 [分支保护](../../../docs/ci-cd.md#分支保护)。本 skill 只串联执行步骤；两者冲突时以文档为准，并先修文档。

## 1. 开 issue

先确认需要 issue：Dependabot 更新和维护者确认的纯机械变更可以免开，其余都开。

`.github/ISSUE_TEMPLATE/` 下的模板是表单。用 `--body-file` 创建时，正文按模板字段写成 `### 字段名` 小节：

- **bug**：错误结果、复现步骤、预期结果、环境、证据与验收；
- **feature**：预期结果、当前问题与证据、验收条件、非目标；
- **research**：核心问题、证据标准、交付结论。

```bash
gh issue create --title "fix: <现象>" --label bug --body-file <正文文件>
```

标题沿用模板前缀（`fix: `、`feat: ` 等）。正文只写可复现的事实和验收条件，不写凭据、token 或本机账户信息。根因已经确认时一并写进去。

## 2. 分支

始终从最新的 main 开分支，不要在主仓库的 checkout 上开发：

```bash
git fetch origin
git worktree add -b <type>/<slug> <worktree 路径> origin/main
```

实现阶段按任务使用对应的 skill，例如 [nano-plugin-development](../nano-plugin-development/SKILL.md)。Agent Note 按 [nano-agent-notes](../nano-agent-notes/SKILL.md) 写，Context 或 Verification 中引用 `#N`。缺陷修复先写能复现失败的永久测试。

## 3. 推送并开 PR

推送前执行 [nano-pre-push-checks](../nano-pre-push-checks/SKILL.md)，然后：

```bash
git push -u origin <type>/<slug>
gh pr create --base main --head <type>/<slug> --title "<type>(<scope>): <summary>" --body-file <正文文件>
```

正文逐节填写 [PR 模板](../../../.github/pull_request_template.md)：

- 第一行写 `Closes #N`，涉及 ADR 时一并写上；
- 验证一节写实际运行的命令和结果；
- 回滚一节写清楚具体做法；
- Checklist 不适用的项注明“不涉及”。

## 4. CI 与审查

```bash
gh pr checks <PR> --watch --interval 60
```

CI 失败时：

- 运行结束后用 `gh run view <run-id> --log-failed` 查看失败日志；
- 单个 job 的日志用 `gh api repos/{owner}/{repo}/actions/jobs/<job-id>/logs` 获取；
- 平台相关的失败先在相同平台复现，不能靠重跑变绿。

修复做成新提交并推到同一分支，按 fast-forward 推送。只有维护者授权改写历史时，才按 nano-pre-push-checks 使用 `--force-with-lease`。

审查使用 [nano-code-review](../nano-code-review/SKILL.md)。审查和修复的轮次写进 PR 正文的验证一节，正文用 `gh pr edit <PR> --body-file <文件>` 更新。

## 5. 合并

必须先得到维护者的合并许可。合并前确认 head 和状态：

```bash
gh pr view <PR> --json headRefOid,mergeStateStatus
gh pr checks <PR>
```

只有 `All checks passed` 为绿、`mergeStateStatus` 为 `CLEAN` 时才合并，并固定已验证的 head：

```bash
gh pr merge <PR> --squash --match-head-commit <sha> \
  --subject "<type>(<scope>): <summary> (#<PR>)" \
  --body "<一段变更摘要>. Closes #N."
```

squash body 只写摘要，不要粘贴所有中间提交的标题。仓库已开启合并后自动删除远端分支。

## 6. 结案

```bash
gh pr view <PR> --json state,mergeCommit
gh issue view <N> --json state
gh issue comment <N> --body "已由 #<PR> 修复（squash 合入 main <短 SHA>）。<结论与权威文档>"
```

关键字没有自动关闭 issue 时，用 `gh issue close <N> --comment ...` 手动关闭。然后：

```bash
git -C <主仓库> pull --ff-only origin main
gh run list --branch main --limit 5
gh run watch <main 上的 CI run> --exit-status
```

main 上的 CI 变红时，按第 1 步新开 issue 和 PR 修复。

## 7. 清理

确认 worktree 干净后，删除 worktree 和本地分支，并清理这次产生的临时文件：

```bash
git worktree remove <worktree 路径>
git branch -D <type>/<slug>
git worktree prune
```

只删除这次流程自己创建的东西；不属于本次流程的分支、stash 和其他工具的 worktree，先问维护者。

## 报告

向维护者报告以下内容：

- issue 和 PR 编号；
- 合并提交；
- CI 结果，包括 PR 的 CI 和合并后 main 的 CI；
- issue 是否已关闭；
- 没有验证的部分；
- 清理了什么、保留了什么。
