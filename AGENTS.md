# AGENTS.md

`nano-harness` 以 Go 为主。修改代码前先读 `docs/architecture.md`；涉及并发、进程、文件、凭据或持久化时再读 `docs/security.md` 和 `docs/testing.md`。

## 当前阶段

仓库尚未承诺稳定 API。优先建立正确、简单的基础：不为尚不存在的调用方增加兼容层、抽象、配置项或状态机。确需破坏性调整时，同一变更中更新全部调用点、测试和文档；禁止静默接受旧格式或错误配置。

## 不得修改参考仓库

`third_party/deepseek-harness` 是只读 Git submodule，只用于架构和流程研究。不要直接修改其中内容，不要复制其许可证未覆盖的代码，不要在主仓提交 submodule 内的脏文件。更新指针必须单独说明上游旧/新提交、变更摘要和重新评估结果，并运行 `make submodule`。

## 常用命令

```bash
make bootstrap       # 安装固定版本的本地工具
make quick           # 格式、模块、vet、race test、架构、submodule
make check           # 正常提交前门禁
make ci              # CI 等价门禁，增加漏洞和发布配置检查
make coverage        # 每个产品源文件 100% coverage
make agent-notes     # Agent Note 格式/PR 携带检查
make skills          # repo-local skills 元数据、契约与链接检查
make change-scope    # 首次提交前检查工作树；有提交后设置 BASE_REF
make build           # 从真实 cmd 入口构建并 smoke test
```

优先运行覆盖变更面的最小检查；提交前运行 `make check`。只有 CI 诊断、发布或明确要求时才重复完整矩阵。

## 项目 Skills

`.agents/skills/` 中的本地 skill 是以下任务的执行入口：运行时组件使用 `nano-plugin-development`；审查使用 `nano-code-review`；推送前使用 `nano-pre-push-checks`；非平凡改动记录使用 `nano-agent-notes`；简化调查使用 `nano-find-simplifications`；文档结构与文案分别使用 `nano-doc-standards` 和 `nano-prose-standard`。触发相关任务时先读对应 `SKILL.md`，但本文件和 `docs/` 仍是规则权威；skill 不得覆盖或放宽逐文件 100% coverage、Agent Note 和所有组件插件化要求。

## 架构

- `cmd/nano-harness` 只负责配置解析、依赖组装、生命周期启动与退出码；业务行为不放在 `main`。
- `internal/core` 保存领域状态、不变量和纯逻辑，只可依赖标准库及其他 `core` 包。
- `internal/app` 保存用例；小接口由消费它的 `app` 包定义，只可依赖 `app` 与 `core`。
- `internal/adapter/<capability>` 实现外部能力，可依赖 `app`、`core`、`platform` 和同一 adapter 子树；不同 adapter 之间不得直接依赖。
- `internal/platform` 提供领域无关的 OS/进程/时钟等薄封装，不得反向依赖领域层。
- `internal/tools` 仅供仓库门禁使用，不进入产品二进制。
- 不创建公共 `pkg/`，除非已有仓外调用方和明确稳定性承诺。
- `go run ./internal/tools/archcheck` 是依赖方向的执行门禁；不能用 lint 例外绕过架构错误。

**所有运行时组件都是插件。** agent loop、session、模型适配器、工具、provider、策略、投影、telemetry 和 UI/协议桥都实现 `internal/core/plugin.Plugin`，由 `cmd` 的有序 composition 启动。纯值类型、DTO、算法和仓库工具不是运行时组件，不包装为空插件。每个插件的全部注册、goroutine、listener、进程和缓存贡献必须通过 `plugin.Scope.Defer` 注册可等待 cleanup；启动失败和 shutdown 按逆序回收，cleanup 失败不得阻止其余 cleanup。禁止绕过 Scope 制造无所有者 side effect。

一个可替换能力必须同时考虑三种角色：消费方接口、provider 插件、consumer 插件/调用方。接口放在消费方，返回具体类型，避免“为接口而接口”。provider 特有配置和传输字段不得泄漏进领域接口。插件生命周期不等于 service locator：依赖仍由构造函数显式注入。

新增行为优先使用已记录的扩展点；核心循环、持久化格式、模型输入或 wire API 的变化必须更新架构文档并增加 ADR。模型可见的信息必须能从权威会话事件重建；先记录成功提交的事实，再更新投影、缓存或 UI。

## Go 代码

- 最低 Go 版本由 `go.mod` 声明；主开发工具链由 `.go-version` 固定。不得无说明提高最低版本。
- 所有 Go 文件由 `gofmt`/`goimports` 格式化；`go mod tidy -diff` 必须为空。
- 包名短、单数、表达职责；避免 `util`、`common`、`manager` 等含混容器。
- 接口保持最小，通常在消费方定义；除非需要 nil、可变实现或生命周期控制，否则接受接口、返回具体值。
- `context.Context` 是可能阻塞或跨边界操作的第一个参数；不得存入长期对象，不得以 `context.Background()` 丢弃上游取消。
- 错误增加操作和稳定标识信息并用 `%w` 保留原因；调用方用 `errors.Is/As`。库代码不 `panic`，不可恢复的进程启动错误由 `cmd` 转为退出码。
- 每个 goroutine 必须有所有者、停止条件和可等待的结束点。关闭必须达到静止状态：先停止新通知，再取消子任务，并等待退出。
- 配置、JSON、RPC、文件、持久化和进程输出在进入类型安全边界时校验；同进程已类型化值不重复做敌对输入校验。
- 部署可变参数必须显式配置并在加载时校验；安全不变量和协议常量保持固定。错误配置尽早失败，不得静默降级。
- 不记录 token、密钥、完整提示词或敏感文件内容。临时目录使用不可预测名称和最小权限；子进程环境使用允许列表或显式清理。
- 注释解释行为、失败方式、所有权和不变量，不复述代码或保留评审历史。导出符号必须有准确的 Go doc。
- `//nolint` 必须指定 linter 并在同一行解释不可消除的原因；禁止全局关闭规则来掩盖局部问题。

## 测试

- 测试与包同目录；行为名使用 `Test<Type>_<Behavior>` 或清楚的表驱动子测试名。
- 单元测试覆盖边界值、错误路径、取消、顺序、并发和资源释放，不只覆盖 happy path。
- 优先真实实现，只替换网络、模型、时钟等昂贵或不确定边界。
- 端到端测试必须走真实 `cmd`/配置入口，并从进程、文件或协议输出验证外部世界；不得只断言被测对象自己的成功文案。
- 测试创建的 goroutine、进程、目录、listener 和数据库由该测试清理，即使失败或超时也必须回收。
- 正常门禁运行 `go test -race -count=1 ./...`。每个产品源文件 statement coverage 必须为 100%；只有不可插桩生成代码等客观例外可通过局部配置排除，并须 Agent Note、替代证据和 reviewer 批准。不要为覆盖率保留无价值分支，优先删除死代码。
- 每个插件必须测试启动贡献、逆序 cleanup、启动失败回滚和 shutdown 后静止；registry 贡献在 scope 关闭后必须不可见。
- 修复缺陷必须先有可复现失败的永久测试。用户、模型、协议或持久化可见变化必须有 assembled/e2e 或 golden 证据。

## 文档与决策

- 代码行为、配置、默认值、错误、wire 字段发生变化时，同一变更更新所属文档。
- 每个非平凡变更在同一 PR 添加或更新 `.agents/notes/` Agent Note；归档 Note 冻结。只有 maintainer 确认的纯机械变更可豁免。
- 架构、持久化、协议、安全模型、依赖方向或发布流程的长期变化还要在 `docs/decisions/` 添加 ADR；Agent Note 不替代 ADR。
- 文档描述当前事实。一个事实只有一个权威位置，其他位置链接引用；生成文件必须标注来源和验证命令。
- 提交信息遵循 Conventional Commits：`feat`、`fix`、`refactor`、`test`、`docs`、`ci`、`build`、`chore`、`perf`、`revert`。

## CI/CD

- PR 必须通过静态检查、lint、race tests、覆盖率、跨平台构建、漏洞扫描、release dry-run 和汇总门禁。
- CI 使用只读默认权限并取消同一 PR 的旧运行；必需检查以 `all-checks-passed` 为唯一稳定汇总名。
- 构建阶段无发布凭据。发布仅允许从与版本匹配的 `v*` tag 手动触发，经 `github-release` Environment 审批后上传构建阶段产生且校验过哈希的同一批制品。
- 依赖、Action、Go 和工具版本由 Dependabot 或专门 PR 更新；更新必须通过完整 CI，不得用浮动 `latest` 作为发布输入。

## 完成标准

变更完成时应满足：运行时组件是可回收插件且位于正确层；错误与取消语义明确；每个产品源文件 100% coverage 且相关测试证明失败模式；Agent Note 和文档/ADR 同步；适用的本地 skill 已执行；`make check` 通过；没有 submodule 脏状态、凭据、无关生成物或被忽略的 lint。
