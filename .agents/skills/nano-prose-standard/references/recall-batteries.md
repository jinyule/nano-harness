# 会话视角泄漏搜索探针

探针只用于提高召回率，每个命中都要语义判断；零命中不能替代阅读高密度文案。命令末尾始终排除只读 submodule、冻结 Note、golden/fixture 和本 skill 自己。

```bash
rg -n --hidden -i 'this PR|this branch|this commit|this stack|reviewer|review round|used to|no longer|for now|should be enough|probably fine' <scope> \
  --glob '!.git/**' --glob '!third_party/**' --glob '!.agents/notes/archived/**' \
  --glob '!.agents/skills/nano-prose-standard/**'

rg -n --hidden 'decision [0-9]+|audit [A-Z][0-9]+|design §|plan §|\bT[0-9]+\b|\bW[0-9]+\b' <scope> \
  --glob '!.git/**' --glob '!third_party/**' --glob '!.agents/notes/archived/**' \
  --glob '!.agents/skills/nano-prose-standard/**'

rg -n --hidden '本 PR|本次改动|这次改动|评审|上一轮|旧版|以前|不再|暂时|应该够|设计稿' <scope> \
  --glob '!.git/**' --glob '!third_party/**' --glob '!.agents/notes/archived/**' \
  --glob '!.agents/skills/nano-prose-standard/**'
```

常见合法命中：描述 PR 工作流的流程文档；`/v1/` 协议名；外部 RFC 或 committed ADR 的 section；runtime 同时存在的旧/新连接；Agent Note 的 Alternatives；issue/TODO；`nolint`/coverage suppression 的真实理由；测量来源。判断标准始终是引用能否在当前仓库解析，以及句子是否完整表达当前契约。
