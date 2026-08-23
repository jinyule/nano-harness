# 安全工程规则

## 信任边界

必须在配置、JSON/RPC、模型/tool 参数、文件、持久化、worker/进程和网络边界校验输入。边界内已由 Go 类型保证的值不重复做敌对校验；这会掩盖真正需要校验的位置。

校验包括：长度/数量/递归深度、枚举、路径语义、编码、重复字段、未知字段策略、超时和完整输出上限。错误配置在加载时或最早可解析点失败。

## 凭据与日志

- 凭据仅来自环境、受保护 secret store 或显式 provider；永不进入仓库、fixture、snapshot、命令行参数和错误文本。
- 子进程默认使用允许列表环境；至少清除匹配 `*KEY*`、`*SECRET*`、`*TOKEN*`、`*PASSWORD*` 的变量。
- telemetry、日志和 session event 分开建模。日志不得包含原始 prompt、Authorization、cookie、完整环境或敏感文件正文。
- CI 使用最小权限 secret；fork PR 不接触发布和 live-provider secret。

## 文件与路径

- 路径在使用点进行 scope 校验；清理后的路径仍须确认位于允许根目录，防止 `..` 和 symlink/junction 逃逸。
- 临时目录使用随机名称与 `0700`，文件使用独占创建和 `0600`；避免可预测的共享 `/tmp` 文件名。
- 可能是 symlink/junction 的路径只 unlink link 本身，不递归跟随删除。递归删除仅用于已确认的真实临时目录。
- 文件大小限制对最终完整字节生效，考虑 UTF-8 多字节、封装和元数据。

## 进程、工具与 sandbox

- 命令执行使用 argv，不拼 shell 字符串；只有明确的 shell 工具才解释脚本文本。
- 进程结果分别返回启动错误、exit code、signal、timeout、stdout/stderr 截断和取消，禁止一个状态覆盖另一个。
- 超时/取消后必须终止完整进程树并等待退出；关闭 listener 后再 kill，避免迟到 callback 写入已释放状态。
- sandbox/approval 决策必须在真正执行操作的位置强制，不能只靠 UI 隐藏、schema omission 或 prompt 提醒。
- 默认最小权限、拒绝未知操作；权限升级需显式用户授权并记录稳定、非敏感审计事实。

## 依赖与供应链

- `govulncheck` 阻断可达高风险漏洞；依赖更新还需阅读上游安全说明和许可证。
- submodule 固定到评审过的 SHA，不执行其中 hooks/postinstall，不把它加入产品 build path。
- Action 和发布工具固定版本并由自动依赖 PR 更新；发布构建与上传分权，上传前重新校验制品哈希。
- 生成物来源可追踪；正式发布前增加 SBOM/attestation 消费验证，而不只“生成一个文件”。

## 安全变更证据

权限、sandbox、路径、凭据、进程环境和持久化加密变更必须包含：威胁场景、允许/拒绝矩阵、绕过路径测试、真实执行点 denial 测试、失败时默认状态和回滚策略。相关设计通过 `docs/decisions/` ADR 评审。
