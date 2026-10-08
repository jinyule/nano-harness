# Linux bwrap 私有 /tmp 挂载顺序与 CI 真实 sandbox 门禁

- Status: implemented
- Date: 2026-10-08

## Context

PR #18 的 GitHub CI 在 ubuntu-latest（Ubuntu 24.04 镜像 `20260927.320`）上失败：`Go 1.26.x / race tests`、`Go 1.27.x / race tests` 与 `Coverage` 都卡在 `TestBash_WorkspaceSandboxAllowsInsideAndDeniesOutside` 和 `TestComposition_SandboxModesAndSwitch/workspace-write`。直接原因是 runner 没有安装 `bwrap`，workspace-write 的 bash 返回 `SANDBOX_UNAVAILABLE`。shell 测试的 skip 条件仍匹配旧文案 `workspace sandbox is unavailable`，文案改为上游 `SandboxUnavailableError` 后不再生效；composition 测试没有 skip，read-only 子测试只检查文件不存在，后端缺失时也会通过。

此前 Linux 后端从未在真实 Linux 上运行，只有 argv 测试。在 OrbStack 的 Ubuntu 24.04 arm64 机器（内核 7.0.11-orbstack，LSM 为 `capability,landlock,yama,bpf`，没有 AppArmor）安装 bubblewrap 0.9.0 后，两项测试仍失败：`bwrap: Can't chdir to /tmp/<test>/001: No such file or directory`。runner 先 `--bind root root` 再 `--bind <owned temp> /tmp`，workspace 位于宿主 `/tmp` 下时被后一个挂载遮住。上游 `bwrapProfileArgs` 的顺序是先 `--tmpfs /tmp` 再 bind workspace。Go 测试的 `t.TempDir` 都在 `/tmp` 下，所以修好安装问题后 CI 仍会因这个缺陷失败；真实用户把 workspace 放在 `/tmp` 下时也会遇到。

本仓 Linux 只实现 `bwrap`，没有 Landlock 后端；不可用错误逐字沿用上游文案，其中的 Landlock 建议在本仓不适用。Ubuntu 23.10 起默认开启 `kernel.apparmor_restrict_unprivileged_userns`，未受 profile 约束的 `bwrap` 在 user namespace 中没有 capability，无法挂载。

## Decision

- Linux workspace-write profile 改为 `--tmpfs /tmp --bind <workspace> <workspace>`，与上游相同。`/tmp` 对每条命令私有、退出即丢弃，不再映射到 owned temp；owned temp 位于 workspace 内，`TMPDIR` 仍指向它，跨命令保留。read-only profile 不变。`Runner.command` 不再接收 temp 参数。
- 真实后端测试先运行与 runner profile 无关的最小探针（Linux `bwrap --ro-bind / / -- true`，macOS `sandbox-exec -p '(version 1)(allow default)' true`），只有探针失败才 skip；探针成功后 runner 的任何失败，包括 `SANDBOX_UNAVAILABLE` 的 runner failure，都是测试失败。这样不改产品的错误分类，本地也不会把 argv 缺陷当作后端缺失。`NANO_HARNESS_REQUIRE_SANDBOX` 非空时所有 skip 条件改为失败。三个包各自保留几行 helper，没有为测试另建共享包。
- 新增 `TestRunner_RealSandboxConfinesWrites`：Linux 上 workspace 显式建在宿主 `/tmp` 下，不依赖 `TMPDIR`；验证 workspace 与 `TMPDIR` 可写、workspace 外和 read-only 写入被拒并带 denial 标记；Linux 另验证 `/tmp` 可写、宿主不可见、下一条命令也看不到。shell 与 runner 测试的拒绝目标改到测试包目录，因为宿主 `/tmp` 下的路径在 Linux 私有 `/tmp` 中不存在，只会得到 ENOENT；检出位于 `/tmp` 下时这两项测试按同一规则 skip 或在 require 模式下失败，不误报 denial 缺失。composition 测试读取已提交的 bash 结果，read-only 必须带 read-only denial 标记，workspace-write 不得是错误。
- 新增 mutation 用例 `sandbox-linux-private-tmp-order`，把挂载顺序换回旧顺序时 `TestRunnerCommand_BuildsSandboxInvocation` 必须失败。
- CI 的 `test`、`coverage` 与 release 源门禁运行 `scripts/setup-linux-sandbox.sh`：apt 安装 bubblewrap；sysctl 为 1 时为 `bwrap` 加载 `flags=(unconfined)` 且只增加 `userns,` 的 AppArmor profile（Ubuntu 文档给出的单程序做法，形状与 apparmor 4.0.1 包自带的 `chrome` profile 相同），不全局关闭限制；最后用 runner 的 workspace-write 挂载执行 `true` 作为探针。测试步骤设置 `NANO_HARNESS_REQUIRE_SANDBOX=1`。`mutation` 的定向用例使用替身 runner，不需要宿主后端。

权威位置：挂载规则在 [ADR-0021](../../../docs/decisions/0021-session-sandbox-modes.md#linux-与-macos) 与[安全规则](../../../docs/security.md#approvalshell-与进程)，宿主前置条件在[开发规范](../../../docs/development.md#linux-sandbox)，skip/require 规则在[测试策略](../../../docs/testing.md#真实-os-sandbox)，CI 步骤在 [CI/CD](../../../docs/ci-cd.md)。本 Note 补充[会话 sandbox 三档 Note](2026-10-06-session-sandbox-modes.md)缺少的 Linux 实机证据，其余契约仍由那份 Note 拥有。

## Consequences

Linux 的 confined bash 与上游一样可以在 `/tmp` 下的 workspace 中运行；命令写到 `/tmp` 的文件不再出现在宿主 owned temp 中，需要跨命令保留的临时文件应写到 `TMPDIR`。私有 `/tmp` 是内存 tmpfs，bubblewrap 0.9.0 无法限制大小，上限约为内存的一半，以前落盘到 workspace 的大临时文件现在占用内存，应改写到 `TMPDIR`。workspace 为 `/tmp` 或 `/` 时 bind 覆盖私有 tmpfs，`/tmp` 就是宿主 `/tmp`，可写且跨命令保留，这符合 workspace 的授权。CI 的 race 与 coverage 现在真正执行 bwrap 路径，后端缺失或损坏会让 CI 失败。开发机缺少或无法运行 `bwrap` 时这些测试仍 skip，并在 skip 信息中给出探针的原始错误；探针只覆盖建立 namespace 与只读 root，`--proc`、tmpfs 或 bind 在某台主机上单独失败时测试会失败而不是 skip。bubblewrap 版本随 runner 镜像的 Ubuntu 仓库变化，没有固定。

CI 的 AppArmor 步骤只能在 GitHub runner 上验证，OrbStack 内核没有 AppArmor；run 37733240682 已证明单程序 profile 在 ubuntu-latest 上有效。若将来的 runner 镜像使该方案失效，setup 探针会先失败；备选方案是在一次性 runner 上执行 `sysctl -w kernel.apparmor_restrict_unprivileged_userns=0`，需要另行决定。

## Verification

- 修复前：OrbStack Ubuntu 24.04（Go 1.27.0 linux/arm64，bubblewrap 0.9.0，ripgrep 15.2.0 aarch64 官方归档）中，`go test -race -count=1 ./cmd/nano-harness ./internal/adapter/tool/shell ./internal/platform/...` 复现 CI 的两个失败，shell 失败的原因是 `bwrap: Can't chdir to /tmp/...`；新测试在 `NANO_HARNESS_REQUIRE_SANDBOX=1` 下以同一 runner failure 失败。
- 修复后：同一机器上 `NANO_HARNESS_REQUIRE_SANDBOX=1 go test -race -count=1 ./...` 全部通过，`NANO_HARNESS_REQUIRE_SANDBOX=1 make coverage` 显示 Linux 上每个产品源文件 100.0%。`-v` 运行确认三项真实后端测试和两个 confined 子测试都是 PASS，没有 SKIP。
- 门禁负例：把 `/usr/bin/bwrap` 移走后，不设变量时三项测试 SKIP（composition 的 read-only 与 workspace-write 子测试 SKIP，full access 仍 PASS）；设置 `NANO_HARNESS_REQUIRE_SANDBOX=1` 时三项全部 FAIL，信息含原始不可用错误。
- `scripts/setup-linux-sandbox.sh` 在 OrbStack 机器上运行成功（sysctl 不存在，跳过 profile）。安装 apparmor 4.0.1 后，`apparmor_parser -Q -K` 解析通过 profile，拼错规则的对照样例被拒。
- `python3 scripts/mutation-check.py --manifest <仅含新用例>`：`sandbox-linux-private-tmp-order` 被 killed。
- macOS：`GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 与 `AGENT_NOTE_BASE_REF=feat/upstream-tool-parity make agent-notes` 通过。
- 复审修复（S1–S3、S5、S6）后，在新的 OrbStack Ubuntu 24.04 arm64 机器上：require 模式下三项真实测试与两个 confined 子测试 PASS；把挂载顺序手工改回旧顺序、不设变量时，三项测试全部 FAIL（composition 的 workspace-write、shell 与 runner），不再 SKIP；仓库副本放在 `/tmp` 下时，shell 与 runner 测试带检出说明 SKIP，require 模式下 FAIL，composition 仍 PASS；`NANO_HARNESS_REQUIRE_SANDBOX=1 go test -race -count=1 ./...` 与 `make coverage`（100.0%）通过，包目录与 `/tmp` 没有残留。macOS 上把 `sandbox-exec` 移出 PATH 后，runner 与 shell 测试 SKIP，设置变量后 FAIL。
- GitHub CI：PR #18 在 `bbabd36` 上的 run 37733240682 全部通过。setup 步骤打印 `granted user namespaces to /usr/bin/bwrap through AppArmor` 与 `bubblewrap 0.9.0`，说明 runner 开启了 `apparmor_restrict_unprivileged_userns`，单程序 profile 生效；Go 1.26/1.27 race 与 Coverage 在 `NANO_HARNESS_REQUIRE_SANDBOX=1` 下通过。
- 尚未获得：Linux x86_64 上的本地运行；`4883e39` 的探针与测试调整在 GitHub runner 上的运行结果。
