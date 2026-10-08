# 工具定义抽象与 Base 文件、搜索、Shell 工具对齐

- Status: implemented
- Date: 2026-10-04

## Context

这是[工具对齐计划](2026-10-04-upstream-tool-parity.md)的 WP1。产品原先注册 `read_file`、`list_files`、`search_files`、`apply_patch`、`run_shell`，每个工具手写 JSON schema、手写解码，并在 `Execute` 内校验参数，所以参数错误也会先触发 approval。`run_shell` 的 `host` 参数和 `apply_patch` 在上游参考提交 `5badb15009ae` 的 Base 组合中都没有对应定义。

上游 `docs/tool-catalog.md` 用工具包的默认配置启动，与 Base 组合不同：Base 关闭 glob 抽样，并挂载 fs/bash sandbox，因此 `write`、`edit`、`bash` 还声明 `sandbox_permissions` 和 `justification`。上游 `defineTool` 的根对象对未知成员开放，并在 `execute` 内校验参数；上游工具包通过 `ctx.systemPrompt` 贡献 `tool:*` 段落。

非目标：spill 存储、先读后写保护、glob 抽样（WP2），后台任务与超时转后台（WP3），多模态结果（WP9），以及其余 Base 工具。

## Decision

会话 sandbox 三档、策略上下文、文件 host 边界与 Linux 联网的当前实施由[WP14 Note](2026-10-06-session-sandbox-modes.md)拥有；本 Note 保留原组件/定义/运行时建立的证据。

搜索 Unicode 参数、JSON framing、诊断预算及取消偏离的补充证据见[对齐 Note](2026-10-06-search-spill-query-parity.md)；本 Note 保留工具定义与运行前提的建立证据。

工具并发预算、参数超限恢复与 Safety 文件策略提示的补充实施见[工具运行时修复](2026-10-06-tool-runtime-upstream-alignment.md)；本 Note 保留定义建立、工具映射与原始验证证据。

长期契约记录在 [ADR-0007](../../../docs/decisions/0007-upstream-base-tool-definitions.md)，当前事实分别归[架构](../../../docs/architecture.md#工具approval-与调度)、[安全](../../../docs/security.md#workspace-文件边界)和[测试](../../../docs/testing.md#模型可见工具目录)文档。本次实施：

文件发布取消与共享路径的物理父目录解析由[文件修复 Note](2026-10-06-file-upstream-alignment-fixes.md)补充；本 Note 保留定义抽象、组合与原始验证证据。

- `internal/app/tool` 改为 `Spec[A]` + `Define` 的定义抽象。`schema.go` 负责有序子集、序列化、校验和 Go 类型检查；`define.go` 负责类型化准备、guidance 和结果类型；`runtime.go` 在批次开始前按 schema 校验并分类所有调用，每个调用轮到时再依次运行 `Check`、approval 和执行，最后统一收尾结果（UTF-8 修复、按 rune 截断、approval 原因截断到 1 KiB）。`Check` 推迟到调用轮次，是为了让它观察同一批次前序调用的效果，例如前一条命令刚创建的 `workdir`。`Invocation` 带有 call ID、turn、step 和调用方 journal，供 todo、jobs、subagent、规划模式等需要写会话事实的工具使用。失败结果统一为上游的 `Error: <message>`，包括 jsonl resume 补写的中断结果。注册清理通过 `*Tool` 指针判断身份，不会因工具值含 func 字段而 panic；工具输出与错误文本都在 rune 边界截断。`Runtime.Catalog` 取代 `Definitions`，同一快照返回 schema 与 guidance；engine 把 guidance 交给 prompt assembler。无效 spec 由 `Register` 拒绝，所以 provider 的启动失败路径保持单一。
- 原 `internal/adapter/tool/workspace` 插件拆为纯值包 `workspace`（root、路径矩阵、sandbox 词汇）和三个插件：`file`（`fs-tools`：read/write/edit）、`search`（`search-tools`：glob/grep）、`shell`（`shell-tools`：bash，拥有 workspace 内的临时目录）。WP2、WP3、WP9 可以分别修改这些包。`cmd` 解析一次 `workspace.Root` 并注入三个 provider。
- subagent 工具迁移到同一抽象；名称和描述不变，schema 去掉根 `additionalProperties:false` 和 `maxItems`。
- `platform/process` 的请求区分可写 `Root` 与工作目录 `Cwd`，删除无人使用的 stdin。stdout/stderr 默认各自保留 64,000 字节尾部，search 的独立 stderr 预算见上述对齐 Note；退出码、信号、超时和 sandbox 拒绝成为结果字段，只有无法启动、sandbox 不可用和调用方取消才返回错误。超时通过 `Cmd.Cancel` 终止进程组，`WaitDelay` 限制后台进程占住管道的时间，运行结束后再次清理进程组。
- `host` 映射为上游升级字段：只有 `bash` 接受 `danger-full-access`；`write`/`edit` 在审批前拒绝它。每次 write/edit/bash 仍需一次性 approval，参数无效或路径不安全时不会提问。
- `glob`/`grep` 按维护者决定依赖 ripgrep，参数、退出码语义、`--json` 解析、上限和错误文案与 `packages/fs/tool-fs-search` 一致。provider 构造时从 PATH 解析 `rg`，`Start` 用 `rg --version` 拒绝低于 15.0.0（上游打包版本）的 ripgrep。ripgrep 以 argv、`--no-config`、allowlist 环境、空 stdin 在 host 模式运行，stdout 上限 20,000,000 字节。为此 `platform/process` 增加 `StdoutLimit`，host 模式可以省略 `TempDir`。纯 Go 的 walker 与 glob 编译代码已删除。CI 的 test、coverage、mutation 和 release build 用 `scripts/install-ripgrep.sh` 安装校验过 SHA-256 的 15.2.0；`make tui-e2e` 把当前 `rg` 所在目录加入被测进程的 PATH。
- composition ID 改为 `fs-tools-v1`、`search-tools-v2`、`shell-tools-v1`、`subagent-tools-v2`。
- prompt 的安全段落改为说明路径按 workspace 解析并拒绝 workspace 外路径；delegation 段落改为不能请求 sandbox 升级。`bash`、`read`、`glob`、`grep` 贡献上游 guidance；`write`/`edit` 的 guidance 依赖观察策略，留给 WP2。
- mutation 用例 `workspace-size` 改为 `read-byte-cap`，`workspace-escape` 指向 containment，`workspace-symlink` 保护写入不跨 symlink。当前路径 resolver 按有限的显式组件序列前进，具体实现见文件修复 Note。

## Consequences

模型看到的六个 Base 工具与上游逐字节一致，后续工作包只需写 `Spec` 即可新增工具，验证和审批前检查不再由每个工具重复实现。

代价和风险：

- 严格的根成员校验、workspace 路径约束和每次执行的 approval 比上游严格。
- ripgrep 15.0.0+ 成为运行与测试前提，发布制品不包含它；用户、开发者和 CI 都要安装，固定版本与校验值需按[开发规范](../../../docs/development.md#ripgrep)手动升级，Dependabot 不覆盖。
- 新建文件改为 `0600`，新建目录改为 `0700`。
- 旧会话按 composition mismatch 拒绝恢复。本仓尚无发布 tag，没有用户会话需要迁移。
- 两份目录 fixture 需要人工维护；参考指针更新时必须重新推导 `upstream-base-tools.json`。

复杂度观察（`make quality BASE_REF=main`，阈值 10，仅观察）：最高的是 `(*Runner).Run`（23，路径与 host 模式校验加进程结局分类）和 `readWindow`（18，流式 UTF-8 校验与行缓冲，窗口记账已拆为 `windowBuilder`）。ripgrep 部分最高的是 `parseMatches`（14，每个 malformed 细节一个分支）和 `(*Provider).run`（13，每种 ripgrep 结局对应一种上游文案）。这些分支对应独立的失败语义，继续拆分只会把状态散到多个函数。新增代码没有跨包重复候选。

## Verification

- `go test -race -count=1 ./...`：通过。
- `scripts/coverage.sh`：每个产品源文件 100.0%。
- `golangci-lint run ./...`（v2.12.2）：0 issues。
- `python3 scripts/mutation-check.py`：八个用例全部 killed，包括新增的 `read-byte-cap`、`workspace-escape`、`workspace-symlink`。
- `make check`：退出码 0，依次覆盖 fmt、mod、vet、race test、architecture、submodule（`5badb15009ae…`，干净）、agent-notes、skills、workflow-tools、lint、coverage、mutation 和 build。
- `make tui-e2e`：编译后的真实二进制、PTY 和 macOS `sandbox-exec` 通过。根会话 11 个工具调用全部成功；三次 approval 均为 `allowed-once`；`written.txt` 的字节为 `EDIT_PROOF\n`，`shell.txt` 为 `SHELL_PROOF`；子会话以 `never` 策略调用 `read`；打断、恢复 replay、私有权限和 lock 清理均通过。
- 门禁反例：把 `read` 的 `offset`/`limit` 声明顺序对调后，`TestComposition_ToolCatalogGolden` 与 `TestComposition_MatchesUpstreamBaseTools` 都失败（键集合相同、顺序不同）；把 upstream fixture 中 bash `timeoutMs` 描述改一个词后，parity 测试失败；恢复后两者通过。
- upstream fixture 由一次性脚本从 submodule 的 `docs/tool-catalog.md`、`glob.ts`、`tool-bash/src/index.ts`、`tool-fs/src/sandbox.ts` 和 `sandbox/src/escalation.ts` 抽取原文生成，未参考 Go 实现；随后与真实 composition 的输出比对，六个工具一致。
- `bash` 在本机通过真实 `sandbox-exec` 验证：workspace 内写入成功，workspace 外写入被拒绝并返回 `[sandbox: file access denied under workspace-write mode]` 和升级提示，目标文件不存在。
- ripgrep：搜索测试运行本机 ripgrep 15.2.0，覆盖修改时间排序、VCS 排除、hidden/ignore/include、`.git` 目录开启 `.gitignore`、上限与预览；`-count=10` 稳定，grep 跨文件顺序按分组排序后比较。脚本化 runner 覆盖退出码 2、信号、超时、启动失败、输出超限和每种畸形 `--json`；版本过低、无法解析、探测失败时 `Start` 失败且不注册工具；`TestRunTUI_FailsEarlyWithoutRipgrep` 在 PATH 不含 `rg` 时得到退出码 1 和安装提示。
- `scripts/install-ripgrep_test.sh` 证明伪造归档（可解包可运行）因校验和不符被拒绝、下载失败和缺少目标目录都失败且不留下 `rg`；本机实际运行安装脚本，得到通过校验的 `ripgrep 15.2.0 (rev e89fff89ac)`。Linux x86_64 与 macOS arm64 归档已下载并本地计算 SHA-256，与官方 `.sha256` 文件和脚本中的值一致；Linux 安装步骤的实际运行要等 CI 验证。
- 未验证：Linux `bwrap` 下的拒绝签名 `read-only file system` 只有单元测试，没有真实运行；Windows 不提供 workspace sandbox，`bash` 在那里会以 sandbox 不可用失败（与原 `run_shell` 一致）；没有进行 live provider 调用。
