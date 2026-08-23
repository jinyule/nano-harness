# nano-harness

[![CI](https://github.com/jinyule/nano-harness/actions/workflows/ci.yml/badge.svg)](https://github.com/jinyule/nano-harness/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

`nano-harness` 是一个以 Go 为主的 agent harness 工程。仓库当前处于基础设施阶段：先固定架构边界、质量门禁和发布约束，再扩展运行时能力。

## 快速开始

```bash
git clone --recurse-submodules https://github.com/jinyule/nano-harness.git
cd nano-harness
make bootstrap
make hooks
make check
```

已有克隆若缺少参考代码仓，执行：

```bash
git submodule update --init --recursive
```

构建并检查当前命令入口：

```bash
make build
./bin/nano-harness version
```

## 仓库结构

```text
cmd/nano-harness/              可执行程序与依赖组装入口
internal/core/plugin/          所有运行时组件共享的 Plugin/Scope 生命周期
internal/core/                 纯领域状态与不变量（按需创建）
internal/app/                  用例与消费方定义的小接口（按需创建）
internal/adapter/<capability>/ 外部能力实现（按需创建）
internal/platform/             与领域无关的 OS/运行时设施（按需创建）
internal/version/              构建身份
internal/tools/                仓库门禁工具，不进入产品制品
docs/                          架构、开发、测试、CI/CD 与决策记录
.agents/skills/                可直接调用的项目工程工作流
.agents/notes/                 每个非平凡改动的实施决策与验证证据
third_party/deepseek-harness/  固定提交的上游参考 submodule
```

## 规范入口

- [架构规则](docs/architecture.md)
- [工程与开发规范](docs/development.md)
- [测试策略](docs/testing.md)
- [CI/CD 与发布规则](docs/ci-cd.md)
- [安全规则](docs/security.md)
- [DeepSeek Harness 分析与取舍](docs/reference-deepseek-harness.md)
- [Agent Notes 规则](.agents/notes/README.md)
- [项目 Skills](.agents/skills/AGENTS.md)
- [贡献指南](CONTRIBUTING.md)

`AGENTS.md` 是面向自动化编码代理和贡献者的精简、强制执行版规则；上述文档说明规则的完整上下文。

## 可复用工程 Skills

`.agents/skills/` 将高频工程任务固化为可触发工作流，而不是复制一份静态规范：

- `$nano-plugin-development`：新增或修改任意运行时组件；强制 Plugin/Scope、真实 composition、逐文件 100% coverage 和 Agent Note。
- `$nano-code-review`：按精确 base/head 审查架构、生命周期、边界和证据。
- `$nano-pre-push-checks`：先识别变更面，再运行最小充分证据与提交前统一门禁。
- `$nano-agent-notes`：编写、审计、取代和归档非平凡改动记录。
- `$nano-find-simplifications`：用生产调用点和 ownership 证据寻找可删除复杂度。
- `$nano-doc-standards`：选择权威文档层级并保持代码事实同步。
- `$nano-prose-standard`：保留完整契约，清理重复和不可解析的会话视角。

例如：`使用 $nano-plugin-development 新增一个模型 provider`。各 skill 引用本仓真实脚本和规则；上游 submodule 中的 `dsh-*` skill 只用于研究，不直接对本仓执行。

## 许可证

本项目采用 [MIT License](LICENSE)。`third_party/deepseek-harness` 是独立 submodule，适用其自身许可证。
