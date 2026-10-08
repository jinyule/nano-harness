# Linux bwrap 私有 /tmp 挂载顺序与 CI 真实 sandbox 门禁

- Status: implemented
- Date: 2026-10-08

## Context

PR #18 的 GitHub CI 在 ubuntu-latest（Ubuntu 24.04 镜像 `20260927.320`）上失败：`Go 1.26.x / race tests`、`Go 1.27.x / race tests` 与 `Coverage` 都卡在 `TestBash_WorkspaceSandboxAllowsInsideAndDeniesOutside` 和 `TestComposition_SandboxModesAndSwitch/workspace-write`。直接原因是 runner 没有安装 `bwrap`，workspace-write 的 bash 返回 `SANDBOX_UNAVAILABLE`。shell 测试的 skip 条件仍匹配旧文案 `workspace sandbox is unavailable`，文案改为上游 `SandboxUnavailableError` 后不再生效；composition 测试没有 skip，read-only 子测试只检查文件不存在，后端缺失时也会通过。

此前 Linux 后端从未在真实 Linux 上运行，只有 argv 测试。在 OrbStack 的 Ubuntu 24.04 arm64 机器（内核 7.0.11-orbstack，LSM 为 `capability,landlock,yama,bpf`，没有 AppArmor）安装 bubblewrap 0.9.0 后，两项测试仍失败：`bwrap: Can't chdir to /tmp/<test>/001: No such file or directory`。runner 先 `--bind root root` 再 `--bind <owned temp> /tmp`，workspace 位于宿主 `/tmp` 下时被后一个挂载遮住。上游 `bwrapProfileArgs` 的顺序是先 `--tmpfs /tmp` 再 bind workspace。Go 测试的 `t.TempDir` 都在 `/tmp` 下，所以修好安装问题后 CI 仍会因这个缺陷失败；真实用户把 workspace 放在 `/tmp` 下时也会遇到。

本仓 Linux 只实现 `bwrap`，没有 Landlock 后端；不可用错误逐字沿用上游文案，其中的 Landlock 建议在本仓不适用。Ubuntu 23.10 起默认开启 `kernel.apparmor_restrict_unprivileged_userns`，未受 profile 约束的 `bwrap` 在 user namespace 中没有 capability，无法挂载。

## Decision

- Linux workspace-write profile 改为 `--tmpfs /tmp --bind <workspace> <workspace>`，与上游相同。`/tmp` 对每条命令私有、退出即丢弃，不再映射到 owned temp；owned temp 位于 workspace 内，`TMPDIR` 仍指向它，跨命令保留。read-only profile 不变。`Runner.command` 不再接收 temp 参数。
- 真实后端测试的 skip 只认稳定分类：runner 层 `errors.Is(err, process.ErrSandboxUnavailable)`，shell 与 composition 层用结果的 `SANDBOX_UNAVAILABLE` 错误码。`NANO_HARNESS_REQUIRE_SANDBOX` 非空时同一分类改为失败。三个包各自保留一个几行的 helper，没有为测试另建共享包。
- 新增 `TestRunner_RealSandboxConfinesWrites`：workspace 在宿主临时目录下，验证 workspace 与 `TMPDIR` 可写、workspace 外和 read-only 写入被拒并带 denial 标记；Linux 另验证 `/tmp` 可写且宿主不可见。shell 测试的拒绝目标改到测试包目录，因为宿主 `/tmp` 下的路径在 Linux 私有 `/tmp` 中不存在，只会得到 ENOENT。composition 测试读取已提交的 bash 结果，read-only 必须带 read-only denial 标记，workspace-write 不得是错误。
- 新增 mutation 用例 `sandbox-linux-private-tmp-order`，把挂载顺序换回旧顺序时 `TestRunnerCommand_BuildsSandboxInvocation` 必须失败。
- CI 的 `test`、`coverage` 与 release 源门禁运行 `scripts/setup-linux-sandbox.sh`：apt 安装 bubblewrap；sysctl 为 1 时为 `bwrap` 加载 `flags=(unconfined)` 且只增加 `userns,` 的 AppArmor profile（Ubuntu 文档给出的单程序做法，形状与 apparmor 4.0.1 包自带的 `chrome` profile 相同），不全局关闭限制；最后用 runner 的 workspace-write 挂载执行 `true` 作为探针。测试步骤设置 `NANO_HARNESS_REQUIRE_SANDBOX=1`。`mutation` 的定向用例使用替身 runner，不需要宿主后端。

权威位置：挂载规则在 [ADR-0021](../../../docs/decisions/0021-session-sandbox-modes.md#linux-与-macos) 与[安全规则](../../../docs/security.md#approvalshell-与进程)，宿主前置条件在[开发规范](../../../docs/development.md#linux-sandbox)，skip/require 规则在[测试策略](../../../docs/testing.md#真实-os-sandbox)，CI 步骤在 [CI/CD](../../../docs/ci-cd.md)。本 Note 补充[会话 sandbox 三档 Note](2026-10-06-session-sandbox-modes.md)缺少的 Linux 实机证据，其余契约仍由那份 Note 拥有。

## Consequences

Linux 的 confined bash 与上游一样可以在 `/tmp` 下的 workspace 中运行；命令写到 `/tmp` 的文件不再出现在宿主 owned temp 中，需要跨命令保留的临时文件应写到 `TMPDIR`。CI 的 race 与 coverage 现在真正执行 bwrap 路径，后端缺失或损坏会让 CI 失败。开发机缺少或无法运行 `bwrap` 时这些测试仍 skip，并在 skip 信息中给出原始错误。

后端缺失和 runner 失败（例如 AppArmor 拒绝 namespace）属于同一稳定分类，本地都按 skip 处理，所以本地 skip 也可能掩盖 runner 缺陷，例如本次的挂载顺序问题。CI 的 require 模式负责发现这类问题。bubblewrap 版本随 runner 镜像的 Ubuntu 仓库变化，没有固定。

CI 的 AppArmor 步骤只能在 GitHub runner 上验证：OrbStack 内核没有 AppArmor，本地只验证了 profile 语法和 sysctl 不存在时的跳过路径。如果 runner 上的 profile 方案无效，探针会失败并报告。备选方案是在一次性 runner 上执行 `sysctl -w kernel.apparmor_restrict_unprivileged_userns=0`，需要另行决定。

## Verification

- 修复前：OrbStack Ubuntu 24.04（Go 1.27.0 linux/arm64，bubblewrap 0.9.0，ripgrep 15.2.0 aarch64 官方归档）中，`go test -race -count=1 ./cmd/nano-harness ./internal/adapter/tool/shell ./internal/platform/...` 复现 CI 的两个失败，shell 失败的原因是 `bwrap: Can't chdir to /tmp/...`；新测试在 `NANO_HARNESS_REQUIRE_SANDBOX=1` 下以同一 runner failure 失败。
- 修复后：同一机器上 `NANO_HARNESS_REQUIRE_SANDBOX=1 go test -race -count=1 ./...` 全部通过，`NANO_HARNESS_REQUIRE_SANDBOX=1 make coverage` 显示 Linux 上每个产品源文件 100.0%。`-v` 运行确认三项真实后端测试和两个 confined 子测试都是 PASS，没有 SKIP。
- 门禁负例：把 `/usr/bin/bwrap` 移走后，不设变量时三项测试 SKIP（composition 的 read-only 与 workspace-write 子测试 SKIP，full access 仍 PASS）；设置 `NANO_HARNESS_REQUIRE_SANDBOX=1` 时三项全部 FAIL，信息含原始不可用错误。
- `scripts/setup-linux-sandbox.sh` 在 OrbStack 机器上运行成功（sysctl 不存在，跳过 profile）。安装 apparmor 4.0.1 后，`apparmor_parser -Q -K` 解析通过 profile，拼错规则的对照样例被拒。
- `python3 scripts/mutation-check.py --manifest <仅含新用例>`：`sandbox-linux-private-tmp-order` 被 killed。
- macOS：`GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 与 `AGENT_NOTE_BASE_REF=feat/upstream-tool-parity make agent-notes` 通过。
- 尚未获得：GitHub ubuntu-24.04 runner（带 AppArmor 限制）上的 profile 与真实 bwrap 运行证据，需要以 PR CI 为准；Linux x86_64 上的本地运行。
