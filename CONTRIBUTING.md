# 贡献指南

## 开始开发

```bash
git clone --recurse-submodules <repository-url>
cd nano-harness
make bootstrap
make hooks
make check
```

若系统同时安装多个 Go 版本，以 `.go-version` 为主开发版本；`go.mod` 中的版本是兼容下限。

## 变更流程

1. 先写清可观察结果、验收条件与非目标。
2. 阅读受影响目录的规范并调用 `.agents/skills/` 中对应工作流；为非平凡工作创建 Agent Note，架构性工作同时提交或更新 ADR。
3. 用最小改动完成行为，补充能证明失败模式的测试。
4. 同步更新配置、错误、API、用户行为对应的文档。
5. 运行 `make check`，需要发布或安全证据时再运行 `make ci`。
6. 提交仅包含一个可独立评审的主题。

## Commit 与 PR

提交标题采用 Conventional Commits，例如：

```text
feat(session): persist admitted user messages
fix(process): wait for child exit during shutdown
docs(ci): explain protected release environment
```

PR 描述必须列出：

- 变更及其原因；
- 对用户、模型、协议、数据格式和安全性的影响；
- 实际运行的验证命令和结果；
- 回滚方式或不需要回滚的原因；
- 相关 Issue/Agent Note/ADR。

不要写“所有测试通过”而不列命令。测试应描述被证明的行为，不描述“代码是正确的”。

## 评审要求

- 合并前所有 required checks 通过，未解决的高优先级评论为零。
- 单维护者阶段必须走 PR、通过 required checks 并解决全部 review conversation；增加第二位 maintainer 后，ruleset 提升为至少一位 Code Owner 批准。安全、持久化、协议和发布变更需要相应领域 reviewer。
- 禁止直接向受保护主分支推送和强制推送。
- 独立变更拆分 PR；同一行为的实现、测试与文档保持在同一 PR。

## 更新参考 submodule

参考仓库更新必须单独提交：

```bash
git -C third_party/deepseek-harness fetch --tags origin
git -C third_party/deepseek-harness checkout <reviewed-tag-or-sha>
git add third_party/deepseek-harness
make submodule
```

PR 中记录旧/新 SHA、上游变更范围、对本仓规范的影响，以及明确决定“不采纳”的规则。不要跟随浮动 branch。
