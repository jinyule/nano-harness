# 工具对齐后的文档权威与 ADR 后续引用

- Status: implemented
- Date: 2026-10-06

## Context

文档在多个工具工作包合并后保留不同的 mutation 计数，README 的工具概览缺少 `read_image`，PTY 概览遗漏读图与长期目标；早期 ADR 也缺少指向后续决定的说明。整理以 `d01c5f4` 为基线，包含 `f64c2dd` 的运行时校验与 `00a9b30` 的目标停止修复。事实核对来源是 `scripts/mutation-cases.json`、`scripts/tui-e2e.py`、真实 composition 的工具 fixture、产品目录与 ADR-0007 至 ADR-0016、ADR-0018。

范围仅为当前文档的一致性与引用。已有 implemented/archived Note 保留原样，ADR 原决定保留；进行中的总体计划与尚未落地的图片存储工作不在此次修改范围内。

## Decision

- [测试策略](../../../docs/testing.md#定向-mutation-与断言有效性)拥有 mutation 的执行与证据契约，用例集合链接到 JSON 清单，不手写总数或复制完整用例列表；CI lane 链接测试策略，ADR-0006 的初始计数标注为历史集合。
- [模型可见工具目录](../../../docs/testing.md#模型可见工具目录)链接两份 fixture，README 的完整能力概览与真实工具集合一致；PTY 场景由[测试策略](../../../docs/testing.md#tui-与真实-cmd)说明，调用序列由脚本断言维护。概览区分前台 one-shot 子代理与 assembled 测试覆盖的完整 fork/后台 continuable 行为。
- README 目录树补齐运行时 skill、目标服务与工具、spill 和设置存储。参考分析链接 skill 入口与固定基线，开发规范链接实测质量基线，移除容易误读为当前诊断计数的重复数字。
- 调试文档按 PTY 脚本的实际资源所有权描述清理：产品进程与临时 fixture 回收，构建产物保留。
- ADR-0002 的工具、subagent 和图片规则分别指向 ADR-0007、ADR-0013、ADR-0015。ADR-0007、0008、0009、0012 添加局部后续说明，覆盖 spill/读图边界、结果图片、owner 释放、one-shot 通知、composition token 和目标消息来源字段；仅增加注释，不重写原决定。
- ADR-0016 与 ADR-0018 双向说明权威范围；ADR-0002、0009、0013 的循环、通知和子代理结果描述链接 ADR-0018 的停止契约。rebase 保留新修复的校验、停止与固定样本证据，mutation 概览仍引用完整清单，不恢复手写计数。

## Consequences

新增工具或 mutation 时，集合与调用序列由对应清单、fixture 或脚本维护，当前文档无需同步手写总数。历史 ADR 与实施 Note 仍可解释原来的范围，后续链接使读者能够找到现行决定。

与本主题重叠的工程证据、工具定义及各工作包 Note 继续拥有各自的历史调查与验证，未被整体取代，不编辑或归档。没有产品行为、Go 注释、fixture、门禁或 submodule 指针变化，不新增 ADR。后续工作包落地时仍需自行更新 owning 文档。

另有与正在修复的规划模式有关的矛盾：ADR-0014 写子代理从不处于规划模式，ADR-0016 把规划投影类比为只读自有事件；基线的 `session.ProjectPlan` 实际遍历全部传入事件。该问题需要产品行为与 assembled 证据一起解决，不在文档整理中改写历史决定或声称已修复。

## Verification

- `git rebase d01c5f4`：成功；CI mutation 行与测试策略概览的冲突保留两边契约，全部用例由清单维护；相对基线只有一个文档提交。
- 静态比对 README 工具集合与 `tool-catalog.json`，核对 PTY fixture 与验证断言、目录树和新增 ADR 引用。
- `make agent-notes`：格式通过；未传 base ref 时不评估提交携带要求。脚本提示 rejected/archived 目录不存在但退出码为 0，未为此创建目录。
- `AGENT_NOTE_BASE_REF=d01c5f4 make agent-notes`：提交携带检查通过，且其他已有 Note 保留原样。
- `make skills`：repo-local skill 契约与链接检查通过。
- Python 3 内联静态检查：当前 Markdown 与新增 Note 的本地链接/fragment 有效；README 工具集合与 fixture 完全相等且无重复；已修改 ADR 的原文行均保留，未触碰预留 ADR。
- `git diff --check`：通过。工作树范围仅含 Markdown，已有 Note 与 submodule 未改。
- 未运行完整 `make check`、PTY、mutation 或 live provider：此次只修改文档，用户明确要求使用文档检查。
