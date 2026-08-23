---
name: nano-prose-standard
description: Use when writing, reviewing, trimming, or restoring prose in a specified nano-harness scope, including Markdown, Go docs and comments, tests, Agent Notes, ADRs, prompts, tool descriptions, diagnostics, logs, CLI strings, and skills; preserves complete contracts while removing repetition, implementation narration, review history, and unresolvable reasoning-session references.
---

# Nano Harness 工程文案标准

写够完整契约，再删除推理流水账、重复和装饰。此 skill 负责语义覆盖与编辑判断；文档归属使用 [nano-doc-standards](../nano-doc-standards/SKILL.md)。

## 输入与排除

必须有明确 scope：用户点名的文件/目录、当前 diff 或实现任务实际触及面都算明确。不要把局部请求扩成全仓文案重写。

始终排除 `third_party/` 和 `.agents/notes/archived/`。submodule 是只读参考；归档 Note 是冻结快照。golden、fixture 和生成文件先修改 owner/scenario，再重新生成，不直接“润色”派生结果。

review/audit 任务只报告；用户明确要求 write/fix/trim 时才编辑。prompt、tool description、diagnostic 和 CLI string 的措辞是行为，必须有相应 test/golden 或说明证据缺口。

## 保留完整命题

编辑前列出每个事实，保留所有相关的：

- actor 与 action；
- condition、timing 和 ordering；
- `must`、`may`、`never` 等强度；
- negative guarantee、exception 和 compatibility；
- ownership、side effect、failure mode 与 consequence。

只缩短字数不是改进。调用点保留使用者必须知道的 behavior/failure/ownership/consequence；架构、算法、历史和完整 rationale 链接到唯一 owner。

## 按位置要求覆盖

- **导出 Go doc：**返回区别、error、side effect、ownership、timing、cancellation 和 durability；不复述签名。
- **包/模块注释：**角色、依赖、责任和非显然架构选择；控制流由代码表达。
- **内部注释：**只解释 invariant、race ordering、资源 owner、安全边界和意外失败。删除逐行 walkthrough 和“这个写法是正确的”式答辩。
- **测试：**只解释 fixture、barrier、平台差异、真实入口或间接观测为何必要；断言 body 自己说明步骤。
- **README/docs：**当前配置、语义、失败、限制、扩展点和可验证入口；一个详细事实只在一个 owner。
- **Agent Notes/ADR：**保留唯一理由、替代、后果和验证；implemented 使用当前时态，archive 不编辑。
- **错误/诊断：**指出失败 subject、违反规则和非显然修复；小写、无句号、不泄漏 secret 或完整输入。
- **skills/agent instructions：**写清触发、范围、禁止动作和什么证据算完成；不要用长篇说服代替 guardrail。

## 清理会话视角泄漏

对可疑段落问：读者只看当前仓库，不看聊天、草稿、PR thread 或未提交 plan，能否解析引用并验证陈述？不能就保留事实、删除会话外壳。

典型问题包括：`decision 7`/`audit B2`/未提交 `§3`；“本 PR/这个 commit/stack 下一层”；“评审认为/上一轮”；“以前/现在/这版”式变更叙事；控制流或测试 walkthrough；“暂时应该够了”且没有 owner 的 hedge。若有 committed ADR/Note/issue，按名字和路径引用；没有 owner 就让事实独立成立。详细校准见 [examples](references/examples.md)，搜索探针见 [recall batteries](references/recall-batteries.md)。

不要误删：可解析 issue/TODO、当前运行时的 old/new 对象、外部 RFC section、真实 measured bound、必要的 `nolint` 理由，以及 present-tense counterfactual regression pin 都可保留。

## 工作流

1. 确认 scope、write authority、base/diff 和适用规则。
2. 读取 owning code/document；使用搜索和字数找候选，但逐段做语义判断。
3. 分类为 keep、add、trim、restore、restructure 或 defer。先改 owner，再更新派生材料。
4. 学到新规则后回查 scope 内 analogous passage；不要为制造 diff 而改写本已清晰的句子。
5. 运行 owning behavior test、文档/Agent Note gate 与 `git diff --check`，确认 diff 没有 `third_party/` 或 archived Note。
6. 报告 scope、修改、刻意保留、borderline case、可见字符串证据和实际检查。
