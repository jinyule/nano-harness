# 目标轮次停止归属与重新授权

- Status: implemented
- Date: 2026-10-06

## Context

`Settle` 的 revision 条件更新保护读取旧结局后发生的重新授权，但错误与输出上限仍使用结算时当前 goal 的 Ref。人类 clear/create 或 pause/resume 先于旧轮次 durable ending 时，旧 error/max_tokens 因而撤销新授权。admission 的停止投影也没有区分归属，会拒绝新 revision。调查基于 `726a95f470246be0b5bde1e15290eaa261cb566c`。

本 Note 补充[目标停止修复](2026-10-06-goal-stop-outcomes.md)，拥有轮次归属和跨 checkpoint 的证据；旧 Note 继续拥有输出原因映射、持久化 outcome、开场失败停止与读后授权交错。同一结算窗口内暂停与解除的累积规则由[结算窗口修复](2026-10-06-goal-settle-window.md)拥有。长期规则由 [ADR-0018](../../../docs/decisions/0018-goal-stop-outcomes.md) 细化，[ADR-0016](../../../docs/decisions/0016-long-running-goals.md) 同步 driver 与 admission。

## Decision

停止投影返回精确 GoalRef。目标轮次的 error/max_tokens 使用开场消息的 goal_id/goal_revision，取消轮次同样只暂停自己的 revision；非目标 turn 的停止使用结束时当前 goal。Settle 在服务锁内比较并解除该 Ref，admission 使用同一投影检查当前 revision。扫描 checkpoint 之前的事件仅恢复 goal 与 turn 开场上下文，不重新应用早先的停止。

修复沿真实 cmd composition 的 goal service、driver 与 journal 接缝，不修改 engine、provider、持久化形状或 composition token。插件顺序、Scope cleanup、watcher 与 goroutine ownership 沿 ADR-0016，不新增运行时 effect。两个 mutation 分别移除轮次归属选择和 admission 的 revision 比较。

## Consequences

新授权无论先于旧轮次结束还是晚于结算读取都得以保留，并能实际准入下一轮。当前轮次的错误/截断和非目标 turn 的停止仍撤销对应授权；没有 goal 的停止不制造目标。没有新增日志、缓存或配置，事件扫描仍为线性成本。

## Verification

- 修复前 `go test -count=1 ./internal/app/goal -run '^TestService_SettleUsesStoppedRoundRef$'` 稳定失败：error/max_tokens × clear/create、pause/resume × 完整日志、开场早于 checkpoint 的八种组合全部把新授权变为 Armed:false。旧 ending 由 channel 屏障推迟到重新授权提交之后，测试 cleanup 释放并等待写入 goroutine。
- 修复前 `go test -count=1 ./cmd/nano-harness -run '^TestComposition_OldRoundEndingPreservesNewGoal$'` 两个 provider → engine → driver → JSONL 场景失败，旧 error 与 max_tokens 均令新 goal active/disarmed。
- `go test -race -count=1 ./internal/app/goal ./cmd/nano-harness -run 'TestService_(Settle|Admit)|TestComposition_(OldRoundEndingPreservesNewGoal|OutputLimitStopsGoalRoundsForEveryProvider)'` 通过。composition 屏障在新 create 提交后才释放旧 SSE；新目标实际发出下一请求，磁盘 create 序号严格小于旧 turn/end。
- `go test -race -count=10 ./internal/app/goal ./cmd/nano-harness -run 'TestService_SettleUsesStoppedRoundRef|TestService_SettleNonRoundStopsDisarmCurrentGoal|TestComposition_OldRoundEndingPreservesNewGoal'` 通过，另覆盖非目标 turn 的停止仍解除当前 goal，以及无 goal 的错误结算。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint-goal-ref make check` 通过：全仓 race tests、架构、submodule、Agent Notes、skills、工作流工具反例、lint（0 issues）、每个产品源文件 100% coverage、全部 56 个 mutation killed，以及真实 cmd 构建与 version smoke。新增两个 mutation 均 killed，清单 ID 无重复且替换位置唯一。
- `make tui-e2e` 通过：真实 binary/PTY、19 次 root 工具调用、附件存储、目标轮次、interrupt/resume、子代理、approval 与 cleanup。Note 相对链接和 `git diff --check` 通过，参考 submodule 无改动，engine/provider 没有 diff。
- provider 证据使用 loopback，无真实远端账户；未运行跨平台 CI 或发布矩阵。
