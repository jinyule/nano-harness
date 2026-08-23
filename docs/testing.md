# 测试策略

绿色测试必须证明发布后的真实行为，而不只是证明 mock 和实现达成一致。

## 测试层级

| 层级 | 命令/位置 | 证明内容 | PR 要求 |
|---|---|---|---|
| 单元/包测试 | `go test ./...`、包内 `_test.go` | 状态转换、边界、错误、顺序 | 所有行为变更 |
| race | `make test` | 受测路径无数据竞争 | 所有 PR |
| 覆盖率 | `make coverage` | 每个产品源文件所有语句被执行 | 逐文件 100% |
| 架构 | `make architecture` | 依赖方向没有漂移 | 所有 PR |
| 组装测试 | 真实构造函数/config | consumer + provider + lifecycle | 新能力/可见行为 |
| e2e/golden | `test/e2e`、`testdata`（按需） | 真实 cmd、协议、文件和 transcript | 用户/模型/wire/持久化变化 |
| live provider | 独立 workflow（按需） | 真实模型/外部 API 可用 | provider 行为变化；无 key 自跳过 |
| release smoke | CI release dry-run | 发布制品可执行 | 所有 PR |

## 覆盖率政策

`scripts/coverage.sh` 排除不进入产品制品的 `internal/tools`，对全部产品包生成同一 coverage profile，并要求每个函数和总 statement coverage 都是 100.0%；因此任何产品源文件的未覆盖语句都会阻断。不可插桩生成代码等客观例外必须局部到具体路径，有同 PR Agent Note、替代测试证据和 reviewer 批准；禁止按包或目录宽泛排除。

100% 覆盖率只证明语句被执行，不证明断言质量。为了补数字而断言实现细节、保留死分支或大面积排除仍然不可接受；优先删除无需求代码，并用错误路径、边界和不变量测试达到门槛。

## 插件生命周期测试

每个运行时组件都是插件，其测试至少证明：唯一 ID 与依赖配置有效；启动后贡献可见；所有 effect 立即登记 cleanup；Scope 关闭后贡献消失；cleanup 逆序且一个错误不饿死其他 cleanup；部分启动失败回滚自身和先前插件；shutdown 返回后 goroutine、进程和 callback 已静止。只有 hand-built plugin unit 仍不足以证明产品入口，产品可见插件还要经过真实 composition 测试。

## 用真实实现替代宽 mock

只替换昂贵或不确定边界：外部模型、网络、时钟和不可控 OS 行为。其下游的 parser、executor、policy、持久化和输出使用真实实现。新能力的组装测试必须经过 `cmd` 使用的构造/配置路径，手工把各对象拼成一个理想状态不能替代组装证据。

修复生产入口问题时，先证明测试在引入该回归时会失败。发布二进制的 smoke 必须运行编译后的 `bin/nano-harness`，不能只 `go run` 或调用内部函数。

## 验证外部世界

- 文件工具：重新读取文件并校验权限/字节，必要时确认未触及文件保持一致。
- 进程工具：独立检查 exit code、signal、timeout、stdout/stderr 和子进程回收。
- 持久化：关闭并重新打开真实 store，再从日志重建状态。
- 协议：从进程外发送/读取真实 wire 字节，校验错误码和关闭语义。
- agent：重放稳定输入并比较规范化 transcript/event log，不依赖 agent 自己声称“已完成”。

## 并发、取消与清理

测试必须拥有自己创建的 server、listener、临时目录、进程、goroutine 和数据库，并通过 `t.Cleanup`/显式 shutdown 回收。关闭测试应证明返回后资源已经静止，而不只是发出了 cancel。

异步测试用 channel/barrier 构造事件顺序；除测试真实 deadline 外，不用 `time.Sleep` 作为同步。分别覆盖取消发生在首个输出之前、部分输出之后和操作提交之后的语义。

## Golden 与 live 测试

用户、模型、协议和持久化可见变化需要 keyless golden/snapshot。golden 更新必须人工审查；CI 只 replay，不写预期文件。

真实 API 测试读取专用、最小权限 secret；secret 不存在时 suite 自跳过并给出清晰原因。无 key 的 plumbing 测试不能代替真实 provider smoke；live 测试也不能替代确定性单元和 golden。
