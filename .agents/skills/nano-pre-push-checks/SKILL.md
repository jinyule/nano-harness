---
name: nano-pre-push-checks
description: Use before pushing or force-with-lease pushing a nano-harness branch, marking a pull request ready, or claiming local checks pass; inspects the exact outgoing Go change, selects focused behavioral evidence, then runs the repository's required architecture, Agent Note, per-file 100% coverage, lint, build, submodule, security, and release gates without hiding failures.
---

# Nano Harness 提交前检查

开发中运行最小充分证据；真正推送前只运行一次 `make check`。CI 负责 Go/OS 矩阵、漏洞和完整 release dry-run，但本地不能把已知失败留给 CI 猜。

## 检查 outgoing change

```bash
git status --short --branch
git rev-parse --show-toplevel
scripts/change-scope.sh <verified-base-ref> [head-ref]
```

远端 PR 或 stack 的 base 必须从实时元数据验证并 fetch；脚本不会猜测或拉取 base。新仓库尚无首个 commit 时可不传 base，脚本只报告 staged、unstaged 和 untracked 范围。base merge、retarget 或 rebase 后重新取范围并重选证据。

## 按变更面选择 focused evidence

- **Go 行为：**先运行 owning package/test，例如 `go test -race -count=1 ./internal/adapter/foo`；共享 contract 变化加入所有实现和 consumer。
- **插件或 composition：**运行 lifecycle、rollback、cleanup error、quiescence 和真实 composition 测试；使用 [nano-plugin-development](../nano-plugin-development/SKILL.md) 的完成条件。
- **import、包移动或架构：**运行 `make architecture`。
- **任何产品 Go 文件：**运行 `make coverage`。逐文件/函数 100.0% 不得通过缩小 profile、`-run`、排除目录或删除有效断言规避。
- **依赖、`go.mod`、工具版本：**运行 `make mod-check`、相关测试、`make vuln`；说明新增依赖相对标准库或现有依赖的净收益。
- **README、docs、Go doc、注释、Agent Note：**运行 `make agent-notes` 和 `git diff --check`，人工检查链接与代码事实；使用 [nano-doc-standards](../nano-doc-standards/SKILL.md)。
- **CI、Action、Makefile、hooks：**运行实际受影响 target；有 `actionlint` 时检查 workflow，不能把 YAML parse 成功当成命令有效。
- **build、cmd、版本、GoReleaser：**运行 `make build`；发布面再运行 `make release-check` 和 snapshot/release smoke。
- **submodule 指针或分析：**运行 `make submodule`，并更新上游 SHA、差异摘要和规则再评估。
- **用户、模型、wire、持久化可见：**运行 owning assembled/e2e/golden。需要 secret 的 live test 只在凭据可用时运行，绝不打印值。

每个非平凡变更都要用 [nano-agent-notes](../nano-agent-notes/SKILL.md) 添加或更新 Note；这不是仅在“文档变更”时才检查的项目。

## 推送前统一门禁

focused evidence 通过后运行：

```bash
make check
git diff --check
git status --short --branch
```

只有用户明确要求 CI 等价复演、正在诊断 CI，或改动横跨漏洞/发布面时才额外运行 `make ci`。不要因为即将 commit/push 就重复刚通过且未被后续编辑失效的相同命令。

## 失败与环境差异

相关检查失败时停止推送，修复或报告 blocker。若判断为环境问题，记录精确命令、版本、平台和失败输出，并证明不受该差异影响的相关证据；不要降低 coverage、跳过 test、全局禁用 lint 或使用 `--no-verify`。只有用户明确授权且理解具体失败时才可绕过 hook。

## 历史改写

普通 push 使用 fast-forward。已获授权的 rebase/修复历史必须先 fetch 并记录远端分支 OID，再使用精确 `--force-with-lease=<branch>:<observed-oid>`；禁止 raw `--force`。推送后确认远端 ref 等于本地 `HEAD`，再读取实时 CI/review 状态。pending 就报告 pending，不能宣称通过。

## 报告

列出 verified base/head、受影响面、实际运行的命令与结果、没有运行的昂贵/live/platform 证据及原因、Agent Note 路径，以及是否存在未提交或 submodule 脏状态。
