# 目标结算窗口累积暂停与解除

- Status: implemented
- Date: 2026-10-06

## Context

第四轮审查（C1）发现，[轮次停止归属修复](2026-10-06-goal-round-stop-ownership.md)把 `settle` 的每个停止改成整体重写 `settlement`。driver 只在 root `WhenIdle` 时结算，两次结算之间可能结束多个 turn：轮次被取消后，若排在它之后的人类 turn 又被取消或出错，先记下的暂停被覆盖，目标停在 active、disarmed。这违背 ADR-0016 的“被取消的轮次暂停”，也削弱权限：模型不能 resume paused 目标，却能在任何人类 turn 中把 active、disarmed 的目标重新 armed。调查基于 `576547a`。

上游对照（`packages/goal/goal-round-driver/src/index.ts`，参考提交 `5badb15009ae`）：

- `turn/end` aborted 且预约处于 claimed/admitted 时只置 `attempt.cancelled`，不解除；预约保留到 idle，此时若目标仍是该 revision、active、armed 就暂停（约 318–331、258–283 行）。因此“取消后再取消”上游仍暂停。
- `agent/error` 与 `max-tokens` 立即解除（约 246–248、331 行之后），idle 时的暂停要求 armed，所以“取消后出错/截断”上游不暂停。
- 审查者的复现 `TestZZ_LaterStopKeepsCancelledRoundPause` 在 error 子用例期望暂停（`d7d199d` 的行为），与上游不同；本修复以上游为准。

## Decision

`settlement` 的 `pause` 与 `disarm` 分别累积：取消的轮次设置 `pause`，每个取消、error 或 `max_tokens` 停止把所属 revision 追加到 `disarm` 列表；error/`max_tokens` 若与待执行暂停同属一个 revision 就撤回该暂停；只有 create/resume 清空两者。`revoked(ref)` 仍按精确 Ref 判断，`Settle` 先尝试暂停再逐个条件解除，B1 的归属规则（轮次按开场 revision、非轮次按结束时当前 revision、新授权不被旧结局撤销）保持不变。

上游在“取消后重新授权再取消人类 turn”时，因旧预约仍保留到 idle 而不解除新授权；本仓沿用 B1 的规则，非轮次停止解除结束时当前 revision，这里的结果是 active、disarmed。改动只在 `internal/app/goal`，不涉及 engine、持久化或 composition。新增 mutation `goal-settle-keeps-pause` 与 `goal-failure-drops-pause`，并把 `goal-admission-stop-owner` 改写为对列表的同义变异。

ADR-0016、ADR-0018 与架构文档补充窗口累积规则，不新增 ADR。

## Consequences

人类中断的目标轮次总以 paused 结束，需要人类显式恢复，除非随后的失败按上游先解除了该 revision。解除改为列表后，同一窗口内不同 revision 的停止不会互相覆盖。扫描仍为线性成本。

## Verification

- 修复前 `go test -count=1 -run SettleWindow ./internal/app/goal/`：`cancelled_then_cancelled` 与 `cancelled_then_cancelled_twice` 失败（phase active），其余六种组合（完成、出错、截断、出错后再取消、重新授权、重新授权后再取消）通过，说明它们已与上游一致。修复后八种组合全部通过。
- 审查者的 `TestZZ_LaterStopKeepsCancelledRoundPause` 在修复后 canceled 子用例通过；error 子用例仍按上游不暂停，未纳入仓库。
- `python3 scripts/mutation-check.py --manifest <goal 用例子集>`：十个 goal 用例全部 killed。
- rebase 到 `255745b` 后，`GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 退出码 0：lint 0 issues，每个产品源文件 100% coverage，清单中 97 个 mutation 全部 killed，真实 cmd 构建与 version smoke 通过。
- `make tui-e2e` 通过：真实 binary/PTY、19 次 root 工具调用，含 `/goal` 轮次完成、interrupt/resume 与 cleanup。
