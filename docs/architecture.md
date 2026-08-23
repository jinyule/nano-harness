# 架构规则

本文定义 `nano-harness` 的目标架构和依赖规则。当前代码已建立命令入口、构建身份、插件生命周期和架构门禁；新增运行时能力必须沿本文扩展，不应创建没有真实消费方的空层或能力接口。

## 设计目标

- 核心 agent 生命周期可以在不修改核心循环的情况下扩展能力。
- 模型、文件、进程、持久化和 UI/协议边界可替换、可测试、可审计。
- 一个行为只有一个权威状态来源；日志、投影和输出能被重放和验证。
- 取消、失败和关闭是 API 的一部分，不是事后补丁。
- 所有运行时组件都是可组合、可逆清理的插件，包括核心循环本身。
- 使用 Go 的小包、小接口和显式构造实现插件架构，不引入平台受限的动态库加载。

## 分层与依赖方向

```text
cmd/nano-harness
       │ 组装
       ▼
internal/adapter ─────► internal/app ─────► internal/core
       │                    │
       ▼                    │
internal/platform ◄─────────┘（只有 adapter 可直接使用 platform）
```

实际强制规则如下：

| 目录 | 职责 | 允许的本仓依赖 |
|---|---|---|
| `cmd/nano-harness` | 读取配置、组装、信号处理、退出码 | 所有 `internal` 层 |
| `internal/core` | 领域值、状态转换、不变量、领域事件 | `internal/core` |
| `internal/app` | 用例、消费方接口、事务/生命周期协调 | `internal/app`、`internal/core` |
| `internal/adapter/<name>` | LLM、文件、数据库、RPC 等实现 | `app`、`core`、`platform`、同一 `<name>` 子树 |
| `internal/platform` | 无领域含义的 OS/进程/时钟设施 | `internal/platform` |
| `internal/tools` | 仓库检查器 | 不进入产品依赖图 |

`go run ./internal/tools/archcheck` 检查上述方向。新增例外必须先改变本文和 ADR，再改变检查器；不能只为让 CI 通过而放宽规则。

## 所有组件插件化

`internal/core/plugin` 是统一生命周期内核。所有运行时组件——agent loop、session store/log、模型适配器、工具与命令、provider、权限与 sandbox 策略、投影、telemetry、后台任务、UI/协议桥——都实现 `plugin.Plugin`，由 `cmd/nano-harness` 按配置生成确定顺序后启动。纯值类型、DTO、算法函数和 `internal/tools` 不产生运行时副作用，因此不是组件，也不包装成空插件。

```go
type Plugin interface {
    ID() string
    Start(ctx context.Context, scope *Scope) error
}
```

- ID 在一个 composition 内唯一且稳定，用于配置定位、诊断和测试；禁止用启动顺序当身份。
- `Start` 只在依赖已由构造函数注入并且配置已解析后执行。插件系统负责生命周期，不充当无类型 service locator。
- 注册、listener、goroutine、进程、临时资源和缓存贡献都是 effect；成功创建后立即用 `scope.Defer` 登记 cleanup。
- cleanup 必须等待其工作静止。Scope 逆序执行全部 cleanup，并聚合错误；一个失败不能饿死后续回收。
- 某插件启动失败时，先清理其部分 effect，再逆序回滚已启动插件；Runtime 进入 `stopped`，禁止半启动继续运行。
- 同一 Runtime 只启动一次；shutdown 幂等。运行期热装卸出现真实需求时扩展 mount/unmount，不另建平行生命周期。
- 新组件必须有生命周期测试：贡献在启动后可见，Scope 关闭后消失，部分启动失败无泄漏，关闭后无 callback/进程/goroutine 残留。

静态编译不削弱“所有组件插件化”：provider 与 consumer 都按 Plugin 协议组合，只是不使用 Go 标准库 `plugin` 包加载 `.so`。未来的外部扩展应通过稳定 wire/WASM/子进程协议或评审后的 registry 实现，不能破坏跨平台发布。

## Go 化的能力接缝

DeepSeek Harness 把一个能力拆成 Service Definition、Provider、Consumer。Go 中保留三角色，但接口由消费方拥有：

```go
// internal/app/turn 包只声明本用例真正需要的能力。
type Model interface {
    Stream(ctx context.Context, request Request, consume func(Chunk) error) error
}

// internal/adapter/deepseek 提供具体实现。
type Client struct { /* provider configuration */ }
```

每个新能力的设计必须回答：

1. 哪个用例消费它，最小接口是什么？
2. provider 如何实现，配置、超时、重试和错误如何归一化？
3. provider 与 consumer 插件如何由 `cmd` 组装，effect 如何登记和回收？
4. 哪些行为是领域事实，哪些只是 provider 诊断？
5. 单元、组装、e2e、协议/golden 分别如何证明它？

只有实现或消费方而没有完整插件调用路径，不算完成的能力。一个接口若只有一个调用者且没有替换、隔离测试或生命周期价值，优先传入具体函数或类型。

## Agent 生命周期

核心循环应保持小而稳定，预计由以下显式阶段组成：

```text
接纳输入 → 记录事实 → 组装模型请求 → 流式响应
       → 执行工具 → 记录结果 → 判断继续/停止 → 提交 turn
```

- agent loop 自身是插件；扩展行为由并列插件挂在已记录的阶段或应用用例上，不直接把 provider 特例写进循环。
- 一个 step 是一次模型请求及其工具执行；一个 turn 包含零个或多个 step。
- waterfall/中间件式调用必须显式调用下一层；终止链路必须返回稳定原因。
- 正交结果独立表达。例如进程可以同时 `TimedOut=true` 且 `ExitCode=0`，不能由一个字段遮蔽另一个。
- 一个异步操作由一个生命周期控制器拥有；启动、取消、完成和资源释放有唯一结算点。

## 事件、持久化与投影

当会话子系统建立后，追加式 session event log 是模型上下文和用户可见会话的权威来源：

- 模型可见的信息必须被记录，且能仅从事件日志重建。
- 事实在操作成功的提交点记录；缓存、搜索索引、标题、UI 和 telemetry 从同一事实派生。
- 持久化事件使用显式类型、schema 版本和稳定字段；结构变化必须有迁移或明确拒绝策略。
- 未知事件的处理是格式协议的一部分：可忽略事件必须在 envelope 中显式标记，不能由读取方猜测。
- 时间、序号、ID 和因果边界由日志所有者分配；跨 wire/文件边界的 ID 使用专用类型，不使用含义不明的裸字符串。
- 查询模型是投影，不反向修改权威日志。

在相关代码出现前，不创建“通用事件总线”或空的 persistence abstraction。第一个真实用例应同时落地事件、存储实现、重放测试和格式文档。

## 配置与默认值

- 配置解析和静态校验在 `cmd`/adapter 边界完成；领域层接收已解析类型。
- 默认值由拥有该决策的组件在单一 `Resolve`/构造阶段应用，运行阶段不散落 `if zero then default`。
- 安全限制、协议常量和存储不变量不可作为普通部署配置关闭。
- 缺失引用、冲突 provider 和不可达依赖尽早报错；不得因错误配置而静默跳过能力。
- 所有超时、大小、并发和保留上限应用于完整结果，包括 envelope、元数据和多字节编码。

## 公共 API 与兼容性

预发布阶段不提供 `pkg/`。只有出现真实仓外消费者，且维护者愿意承担 Go 1 兼容承诺时，才把最小稳定表面移入 `pkg/`。内部重构可以破坏内部 API，但同一 PR 必须原子更新调用方。

以下变化即使预发布也需要 ADR：持久化格式、wire 协议、插件/能力发现机制、安全模型、最低 Go 版本、包依赖方向、发布制品集合。

## 架构完成条件

新增能力合并前应具备：消费方最小接口、provider/consumer 插件、真实 composition 入口、可逆 effect、明确的取消/关闭语义、错误归一化、配置校验、逐文件 100% 单元覆盖、真实插件组装测试、Agent Note，以及对应文档。模型或协议可见时再增加 golden/e2e 证据。
