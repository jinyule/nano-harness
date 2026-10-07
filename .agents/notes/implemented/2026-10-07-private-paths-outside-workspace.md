# 凭据、设置与会话根不得经 workspace 暴露

- Status: implemented
- Date: 2026-10-07

## Context

最终整体审查的 Blocker（Fable 发现，astra 交叉核实）：`normalizeConfig` 只要求 spill 根和附件根与 workspace 互不包含，`--root` 却可以包含 `credentials.yaml`、`settings.yaml` 或 session root。默认 workspace-write 下，`read` 与 `grep` 读取 workspace 内文件无需 approval，`web_fetch` 也无需 approval。astra 用合成凭据和 loopback 复现了完整链路：凭据被读进模型请求，再经 `web_fetch` 的查询参数到达受控接收端，期间没有 approval 记录。读取风险在 main 上已存在，`web_fetch` 外传通道是本分支新增的。默认值都位于 `<用户配置目录>/nano-harness`，所以 `--root ~`（ADR-0008 写到过这种用法）就会触发。

astra 复审（基线 `92e3599`）发现第一版的包含判断只比较字符串：macOS 默认的大小写不敏感文件系统上，`--root …/workspace` 与 `--credentials …/WorkSpace/credentials.yaml` 指向同一目录却被接受，真实组装中 `read` 把合成 API key 送进下一次模型请求，没有 approval 记录。Fable 复审确认并扩大了范围：macOS firmlink（workspace 用 `/System/Volumes/Data/...`、凭据用常规拼写）同样放行。`EvalSymlinks` 只解析链接，不规范化大小写或 Unicode 拼写，也不解析 firmlink。

另一个问题：`parseTUIConfig` 返回 `normalizeConfig` 的错误时不打印，`runTUI` 只以退出码 2 结束，现有 spill/附件根冲突的提示从未到达用户。

非目标：在工具执行点维护私有文件排除名单；保护 workspace 内其他工具的秘密；审批 ID 恢复、取消卡死等同轮其他 Blocker。

## Decision

- `normalizeConfig` 把 session root、spill 根、附件根、凭据文件、设置文件放进同一张私有位置表，用 `separatePrivatePath` 逐项检查：
  - 对每个路径已存在的最长前缀解析链接，路径存在时也解析最终链接；
  - 目录（三个 store 根）与 workspace 不得互相包含，tools 既看不到其中条目，也不能在它旁边写入；
  - 文件只要求解析后不在 workspace 内。workspace 位于文件所在目录之内时不暴露任何东西，所以放行，不对文件的父目录做双向互斥。
- 包含关系由 `within` 判断：先比较拼写，再把目标及其已存在的各级祖先与目录按 `os.SameFile` 比较。大小写或 NFC/NFD 规范化不同的别名、firmlink 与 bind mount 因 dev/inode 相同而被识别，而大小写敏感的文件系统上不同拼写仍是不同目录，没有用 `strings.EqualFold` 一类拼写规则。目标中未创建的后缀不可能是已存在的 workspace；不存在的 store 根也不包含已存在的 workspace，所以不需要为它们另设规则。读取身份失败（非不存在）时启动失败。
- 全部冲突在一条错误中列出，每项写明应改用的 flag：home workspace 会同时命中全部默认位置。`parseTUIConfig` 把配置错误打印到 stderr 后再以退出码 2 返回。
- 错误发生在组装之前，不启动任何插件，不创建会话、锁或设置文件，也不改写已有凭据。
- 约束写入拥有凭据与会话的 [ADR-0002](../../../docs/decisions/0002-provider-neutral-agent-harness.md)。[ADR-0008](../../../docs/decisions/0008-tool-output-spill-and-observation-policy.md) 的 home workspace 示例补上另外三个 flag。security、README 与开发文档的数据目录表同步更新。没有新 ADR、新组件或新配置项，模型可见文本与 composition 不变。

## Consequences

只靠选错 `--root` 已经无法让 API key、OAuth refresh token 或其他会话的 transcript 进入模型请求。以 home 作 workspace 的部署现在需要显式移出五个私有位置；错误一次列出全部 flag。配置错误现在会显示原因，不再静默退出。

这项检查只保护 nano-harness 自己的文件：home workspace 中的 `~/.ssh` 或其他工具的令牌缓存仍可被无需 approval 的 `read`/`grep` 读取，并经 `web_fetch` 发出。security 与 README 写明不应把含秘密的目录作为 `--root`。启动后再把私有位置移进 workspace（例如改链接）不受检查，这与 spill/附件根的既有限制相同。

## Verification

- 修复前：`go test -count=1 -run 'TestNormalizeConfig_KeepsSecretsAndSessionsOutsideTheWorkspace|TestRunTUI_RefusesAHomeWorkspaceHoldingTheDefaultPrivateFiles' ./cmd/nano-harness/` 中，9 个应拒绝的子用例全部被接受，入口测试拿到退出码 0，stderr 也没有提示；4 个放行用例通过。输出在被忽略的 `.cache/fx/red.log`。
- 修复后两个测试通过：
  - 拒绝：尚未创建、经链接目录到达、最终链接指向 workspace 文件的凭据与设置文件；位于 workspace 内、等于 workspace、经链接进入或包含 workspace 的 session root。
  - 放行：workspace 旁的凭据文件；与 workspace 共享前缀的文件和目录。
  - 入口：真实 `run` 以 home 作 `--root`，返回 2，stderr 列出三个 flag，不启动 runtime，配置目录只剩原有凭据且内容不变。
  - 两个既有测试把私有路径从 workspace 内移到同级临时目录，因为它们原本就依赖这类被放行的布局。
- 新增 3 个 mutation（只检查目录、把 session root 当作文件、吞掉配置错误输出），用单独清单运行 `scripts/mutation-check.py` 全部被拒绝。
- 大小写别名修复前：`go test -count=1 -run 'TestNormalizeConfig_RejectsCaseAliasesOfTheWorkspace|TestNormalizeConfig_RejectsNormalizationAliasesOfTheWorkspace|TestNormalizeConfig_JudgesContainmentByFileIdentity' -v ./cmd/nano-harness/` 在本机 APFS 上 5 个大小写子用例、NFD 用例与注入身份的 3 个拒绝用例全部失败（被接受），两个放行用例通过；输出在被忽略的 `.cache/fx2/red.log`。修复后全部通过，`within` 与 `separatePrivatePath` 的语句覆盖率为 100%。firmlink 测试 `TestNormalizeConfig_RejectsFirmlinkAliasesOfTheWorkspace`（凭据或 workspace 经 `/System/Volumes/Data`、session root 经 firmlink 包含 workspace）在修复前代码的 Go overlay 上三个子用例全部失败，修复后通过（`.cache/fx2/firmlink-red.log`）。原 mutation `private-files-stay-outside-workspace` 改指新代码；新增 3 个 mutation（跳过身份比较、只比较目标本身、反向检查只比较拼写）与原有 3 个一起用单独清单运行，全部被拒绝。基于 `92e3599` 的 `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check`（lint 0 issues、逐产品文件 100% coverage、全部默认 mutation killed）与 `make tui-e2e` 通过。本机为大小写与规范化均不敏感的 APFS，并有 `/System/Volumes/Data` firmlink，真实文件系统测试实际运行；Linux 上它们按说明跳过，由注入身份的测试覆盖。Linux 的大小写不敏感挂载、bind mount 与 Windows 只由同一身份逻辑推断，没有原生运行证据。
- 基于 `47af5a2`（rebase 跨过审批 ID 与附件 observer 修复，mutation 清单按并集合并后重跑）：`GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 通过，包括全仓 race、逐产品文件 100% coverage、架构、Agent Note、lint 0 issues、全部默认 mutation 与真实 cmd build/smoke；`make tui-e2e` 通过（e2e 的私有位置本就与 workspace 同级）。随后 rebase 到 `7dad11b`（只改测试夹具与文档）后重跑 `make check` 仍通过，tui-e2e 沿用 `47af5a2` 上的结果。没有 live provider 或其他操作系统的证据，macOS 默认配置目录的情况只通过 hook 模拟验证。
