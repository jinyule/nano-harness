# 工程文案校准示例

这些例子用于识别原则，不是可复制模板。

## 保留 ownership 与完成时点

过度缩写：`关闭时取消 provider。`

完整契约：`Runtime 先请求 provider 取消，再关闭其子 Scope；provider 必须等待 worker 退出后才能让 cleanup 返回。`

actor、顺序、owner 和完成保证是四个独立事实。

## Go doc 写 caller-visible failure

过度缩写：`State 返回当前状态。`

合适：`State returns the current lifecycle state. It is safe to call concurrently with Start and Shutdown.`

若并发保证并不存在，就不能为“完整”而添加；必须先从实现和 test 证明。

## 注释不复述控制流

泄漏：`先检查 context，然后创建 listener，最后登记 cleanup。`

合适：如果代码已经表达这些步骤就删除。若关键事实是 publication 顺序，写：`Register cleanup before publishing the listener so startup rollback cannot leak it.`

## 100% coverage 不等于测试充分

过度缩写：`Tests cover the plugin.`

合适：`Unit tests pin partial-start rollback, reverse cleanup after one cleanup error, and quiescent shutdown; an assembled test starts the plugin through the cmd composition path.`

保留场景和真实入口，不写 fixture 文件清单。

## Agent Note 写现在的事实

泄漏：`本 PR 将会把所有组件改成插件，评审第二轮决定使用 Scope。`

合适：`所有运行时组件实现 plugin.Plugin；Scope 拥有组件创建的可逆 effect，并在失败或关闭时逆序回收。`

决定理由写入 `Decision/Consequences`，不记录谁在哪一轮说过。

## 保留 measured provenance

不完整：`上限为 4 MiB，当前最大样本为 3.1 MiB。`

合适：`4 MiB 上限来自测量：当前最大生成样本为 3.1 MiB。`

若数字只是猜测，不得改写成 measured；应补测量或明确配置依据。

## 当前运行时 old/new 不是历史叙事

可保留：`旧连接完成 draining 后，新连接才开始接收请求。`

这里的旧/新是同一时刻存在的运行对象，不是仓库版本史。

## 修复历史改为当前反事实

泄漏：`这里以前会在 multibyte ID 上截断错误。`

合适：`如果在 rune 边界前按 byte 截断，multibyte ID 会产生无效 UTF-8。`

保留回归条件与结果，删除不可定位的版本故事。
