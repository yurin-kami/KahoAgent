# PDF 解析实现方案（方案 A：Go 库嵌入）

> ## ⚠️ 本方案已被否决，不执行
> **执行方案见 [`pdf-plan-b.md`](pdf-plan-b.md)（MinerU 离线预处理）。**
>
> 保留本文件的原因：它是选型对照的证据，记录了「如果必须用 Go 库」
> 需要付出什么代价 —— 自写字号聚类、表格区域识别、抽取质量自检三整节。
>
> 否决理由：本方案默认了一个**未经论证的前提** ——「PDF 解析必须编进 Go
> 二进制」。而入库实际是离线动作（`ingest_corpus` 带 `corpus` build tag，
> 不进镜像、不进 CI、不在生产请求路径上），这个前提不成立。
> 前提一旦不成立，§1.5、§4、§5 三节就都是为迁就能力不足的库而写的补偿性代码。
>
> **唯一仍然有效的部分是 §6.3.1（docID 碰撞 bug）** —— 它与解析器选择无关，
> 已作为阻塞项写进方案 B 的步骤 0。

---

> 状态：**已否决**。
> 关联：`BACKLOG.md` 待评估项、`IMPLEMENTATION.md:1841` Phase 6 可选项第 2 位。

---

## 0. 先说结论

这不是「加一个 parser」。真正要解决的是**如何在没有标题标记的格式里重建 `heading_path`**。

现有链路对 `heading_path` 的依赖是硬的：

```
internal/rag/chunker/markdown.go:107   写入 MetaHeadingPath
internal/agent/tools/rag_query.go:190  读出来当引用出处
migrations/001_chunks.sql              metadata JSONB 落库
```

Markdown 的 `#` 是**显式结构标记**，扫一遍正则就有层级。PDF 没有这个东西 ——
PDF 是**排版格式**，它只记录「在坐标 (x,y) 用 18pt 的黑体画这几个字」。
「这是标题」是人眼从字号、粗细、留白里读出来的**视觉约定**，不是文件里的数据。

所以本方案的主体是一个**字号聚类推断层级**的算法，PDF 文本抽取本身反而是最简单的一步。

三个必须先接受的前提：

1. **推断会出错**，需要可降级（推不出层级就退化成 PlainChunker）
2. **中文 PDF 有真实的抽取失败风险**（见 §2 的选型证据），必须有检测和拒绝机制
3. **golden set 现在是 77 条纯 Markdown**，加 PDF 会改变评估基线，
   不能直接和历史数据对比（这条最容易被忽略）

---

## 1. 目标与非目标

### 做

- 文本型 PDF 的文本抽取（`internal/rag/parser` 新包）
- 基于字号的标题层级推断，产出与 Markdown 等价的 `heading_path`
- 抽取质量自检：乱码 / 空文本 / 扫描件 → 明确失败，不入库
- 接入 `ingest_corpus` 离线入库路径（**不碰 Kafka**，理由见 §6.2）
- 独立的 PDF golden set，与现有 77 条**分开统计**

### 不做（写清楚，避免范围蔓延）

| 不做 | 理由 |
|---|---|
| 扫描件 OCR | 要么上云 OCR（外部依赖 + 花钱），要么本地模型（几百 MB）。当前镜像已 110MB |
| **表格结构还原** | **改主意了，见 §1.5 —— 分级做，不是完全不做** |
| 多栏排版重排 | 库有 `GetTextByColumn`，但判断「这页是几栏」本身就要启发式，先不碰 |
| 公式 / 图片说明 | 同上 |
| 加密 PDF | 直接报错拒绝。库对 AES-128 有已知 open issue（#82） |

---

## 1.5 表格：分三级，不是一刀切

初版写「不做表格」，理由是「还原表格是独立课题」。这个理由**只对第三级成立**，
把三件不同难度的事混成一件砍掉了 —— 修正如下。

### 三个层次

| 级别 | 做什么 | 难度 | 结论 |
|---|---|---|---|
| **L1 不污染** | 表格文本不能串行混进正文 | 低 | **必做** |
| **L2 可检索** | 表格内容以线性文本入库，能被召回 | 中 | **做** |
| **L3 结构还原** | 还原成行列矩阵 / Markdown 表格 | 高 | **不做** |

### L1：不做的代价是错误，不是缺失

这一级根本不是「功能」，是**正确性**。

PDF 里的表格文本是按绘制顺序散落的。一个两列表格：

```
实际布局              抽取出来（按绘制顺序）
┌────────┬────────┐
│ 缓存穿透 │ 布隆过滤器 │   →  "缓存穿透 布隆过滤器 缓存击穿 互斥锁"
│ 缓存击穿 │ 互斥锁    │
└────────┴────────┘
```

如果不管，这段会作为正文的一部分被 embed。**「缓存穿透 布隆过滤器 缓存击穿 互斥锁」
这个语义混合体，正好是 golden set 里 confusable 那 8 条要区分的东西。**
它会成为一个同时匹配三种缓存问题的高分噪声块 —— 对现有最难的一类查询造成直接伤害。

所以 L1 不是可选项。好在成本不高：`Page.Content()` 返回的
`Content{ Text []Text; Rect []Rect }` 里 **`Rect` 就是页面上画的线段和矩形**。
落在 Rect 区域内的文本 = 表格文本。这是库直接给的数据，不需要推断。

### L2：线性化，够用

识别出表格区域后，按行分组、单元格间用 ` | ` 连接：

```
缓存穿透 | 布隆过滤器
缓存击穿 | 互斥锁
```

不还原结构，只保证**每行的语义是自洽的**。这样「缓存击穿怎么解决」能召回到
「缓存击穿 | 互斥锁」这一行，而不是那坨混合噪声。

对 RAG 来说这就够了 —— 检索要的是「这段文字和查询相关吗」，不是「第 3 行第 2 列是什么」。

### L3：为什么真的不做

还原结构要处理：合并单元格、无边框表格（靠对齐推断）、跨页表格、嵌套表格。
每一项都是独立的启发式，而**收益在 RAG 场景下接近零** —— 
除非要做「表格问答」（查询第几行第几列），否则线性化和结构化的检索效果没有区别。

这才是「独立课题」这个理由真正适用的地方。

> 修正记录：初版把 L1/L2/L3 混为一谈直接砍掉，是个错误。
> L1 不做会产生**错误数据**（污染 confusable 类查询），
> 和 L3 的「功能缺失」性质完全不同。**分不清「缺功能」和「出错误」，
> 是排优先级时最容易犯的错。**

---

## 2. 选型：为什么是 `ledongthuc/pdf`

四个候选实测核对（版本、许可证、cgo 情况均已核对 pkg.go.dev）：

| 库 | 许可证 | cgo | 关键结论 |
|---|---|---|---|
| **`ledongthuc/pdf`** | BSD-3 | ❌ 纯 Go | **选它**。有字号信息，见下 |
| `dslipak/pdf` | BSD-3 | ❌ 纯 Go | 是前者与 `rsc/pdf` 的合并 fork，但 **go.mod 标记 not valid**，最后发布 2024-01 |
| `gen2brain/go-fitz` | **AGPL-3.0** | ✅ 绑 MuPDF | **直接淘汰**，两条理由都是硬伤 |
| `pdfcpu` | Apache-2.0 | ❌ 纯 Go | `extract --mode content` 吐的是 **PDF 语法**不是纯文本，不是干这个的 |

### go-fitz 为什么必须排除

抽取质量它最好（MuPDF 是业界标准），但：

1. **AGPL-3.0 有传染性**。这是个要挂在简历上给人看的公开仓库，
   引入 AGPL 依赖意味着整个项目的分发都要受约束。这是法律问题不是技术偏好。
2. **第二个 cgo 依赖**。项目已经因为 gojieba 踩了四个 cgo 坑
   （见 `docs/spike-evidence/` 与那篇博客）。再加一个 C 库意味着：
   Dockerfile 要再装一套构建依赖、运行时要再带一批 `.so`、
   镜像从 110MB 继续涨、Windows 本地开发再多一个装不上的东西。
3. 而且它的**预编译库不含 CJK 字体**（官方文档明说），
   要中文支持得用 `extlib` 自己编 MuPDF —— 那构建复杂度就失控了。

### 选定库的决定性优势：它给字号

这是整个方案能成立的关键。`ledongthuc/pdf` 的 `Text` 结构体：

```go
type Text struct {
	Font     string   // 字体名
	FontSize float64  // 字号（磅）
	X, Y     float64  // 坐标
	W        float64  // 宽度
	S        string   // UTF-8 文本
}
```

有 `FontSize` 才谈得上推断标题层级。如果库只返回一整坨字符串（比如 go-fitz 的 `Text()`），
那 `heading_path` 就只能是空的 —— PDF chunk 会永远比 Markdown chunk 少一半信息。

API 用 `Page.GetTextByRow()`，它按 Y 坐标分行并在行内按 X 排序，
省掉自己做坐标聚类。

### ⚠️ 已知风险（必须写在这，不能等踩了再说）

`ledongthuc/pdf` 有**未修复**的 open issue，且正中中文要害：

- **#55**：不支持 `UniGB-UCS2-H` 编码 —— 这是中文 PDF **最常见**的 Type0 字体编码。
  报告里 `江苏银行(交易扣款回单)` 抽出来是 ``l_n/9 6]ze/L5033``。
  关联 PR #56 至今未合。
- **#60**：部分文档内容乱码
- **#67**：XObject 里的文本抽不出来
- **#82**：AES-128 加密 PDF 报 invalid password

**这不是「可能会有问题」，是已经有人报了、且没修。**

所以 §4 的质量自检不是锦上添花的防御性编程，**它是这个方案能不能用的前提**。
必须假设「一部分中文 PDF 就是抽不出来」，设计上要能干净地识别并拒绝，
而不是把乱码悄悄写进向量库 —— 那会污染检索结果，且极难排查
（检索出来一段乱码，你根本不知道是分块问题、embedding 问题还是抽取问题）。

---

## 3. 架构：新增一层 Parser，不动 Chunker 接口

### 现状

```
raw []byte ──▶ Chunker.Chunk(ctx, docID, raw) ──▶ []rag.Chunk
```

`rag.Chunker` 的注释写着「本文件是 Phase 1-2 的接口契约，定稿后不应修改」。
不改它。

### 方案

在 Chunker **前面**加一层，把 PDF 归一化成 Markdown 文本，然后复用现有的
`MarkdownChunker` —— 层级推断的产出直接表达成 `#` / `##`，
下游一行代码都不用改。

```
                    ┌─ .md  ─────────────────────────┐
raw []byte ─ 路由 ─┤                                 ├─▶ MarkdownChunker ─▶ []rag.Chunk
                    └─ .pdf ─▶ PDFParser ─▶ md text ─┘
```

**为什么归一化成 Markdown 而不是直接产 Chunk：**

1. 复用已经过测试的 `scan()` 层级栈逻辑（含代码围栏处理、层级跳跃补位）
2. 层级推断的结果**人可以直接看**（打印出来就是 Markdown），
   调试和验证成本极低 —— 这对一个必然会出错的启发式算法很重要
3. 未来加 DOCX / EPUB 走同一条路

### 新增文件

```
internal/rag/parser/
    parser.go          Parser 接口 + 按扩展名路由
    pdf.go             PDF → Markdown
    outline.go         字号聚类推断标题层级（核心）
    quality.go         抽取质量自检
    *_test.go
    testdata/          手工构造的 PDF 样本
```

### 接口

```go
// Parser 把非纯文本格式归一化为 Markdown。
//
// 为什么产出 Markdown 而不是直接产 []rag.Chunk：
// 分块策略（层级切分、超长节二次切分、heading_path 维护）已经在
// MarkdownChunker 里实现且测过，没有理由为每种新格式重写一遍。
// 归一化的另一个好处是产物可读 —— 层级推断是启发式的，必然出错，
// 能直接肉眼看结果对调试至关重要。
type Parser interface {
	Name() string
	// CanParse 按扩展名判断，不读内容（路由发生在读文件之前）。
	CanParse(filename string) bool
	// Parse 返回 Markdown 文本。抽取质量不达标时返回错误而非降级 ——
	// 把乱码写进向量库比拒绝入库糟糕得多。
	Parse(ctx context.Context, raw []byte) ([]byte, error)
}
```

---

## 4. 核心算法：字号 → 标题层级

### 4.1 思路

人怎么看出「这行是标题」？**它比正文大**。就这么简单，但要把它变成代码得处理几个现实问题。

分四步：

**① 统计字号分布，找出正文字号**

```go
// 出现次数最多的字号就是正文 —— 一篇文档 90% 的字符是正文。
// 用「字符数加权」而不是「行数」：标题行数可能不少，但字数一定少。
```

这一步的关键是加权方式。按行统计的话，一份目录页全是短行的文档会把统计带偏。

**② 大于正文字号的，是标题候选**

**③ 把候选字号聚类，映射到层级**

不能直接「字号种类数 = 层级数」。真实 PDF 里 17.9pt 和 18.0pt 是同一级
（浮点误差、不同字体的度量差异），但会被当成两级。

所以要**按容差聚类**：

```go
// 容差取 0.5pt。依据：常见排版的相邻标题级差是 2pt 以上
// （如正文 10.5 / 三级 12 / 二级 14 / 一级 18），
// 0.5pt 足够合并同级抖动而不会误合并相邻级。
// 这个值需要在真实样本上验证，不是拍脑袋定的 —— 见 §6 验收。
const sizeTolerance = 0.5
```

聚类后按字号**降序**排列，最大的是 H1，次大是 H2，以此类推。

**④ 层级数封顶**

```go
// 超过 6 级的部分全部压到 6 级。Markdown 只有 6 级，
// 而且 PDF 里出现 7 种以上的大字号，几乎必然是把
// 封面标题、页眉、图注误判成了标题 —— 这时候多分级只会让结构更乱。
const maxLevel = 6
```

### 4.2 必须处理的干扰

这部分是算法能不能用的分水岭。前面的主逻辑很直白，坑全在这里。

| 干扰 | 现象 | 处理 |
|---|---|---|
| **页眉页脚** | 每页固定位置重复出现，字号可能偏大 | 按 Y 坐标落在页面上下 8% 区域 + **跨页重复**双条件剔除。单靠位置会误杀首页大标题 |
| **页码** | 每页一个孤立数字 | 纯数字且成行 → 丢弃 |
| **封面** | 整页超大字，会独占 H1 把全文压成一节 | 第 1 页若文本量 < 全文 3%，只取最大字号那行当文档标题，其余丢弃 |
| **图表标题** | 「图 3-1 架构图」字号可能大于正文 | 匹配 `^(图\|表\|Figure\|Table)\s*[\d-.]+` → 不作为标题 |
| **加粗正文** | 正文里一句加粗，字号相同 | 只看 FontSize 不看 Font 名，天然免疫 |
| **跨行标题** | 长标题折行，被拆成两个候选 | 相邻行字号相同且 Y 间距 < 1.5 倍行高 → 合并 |

### 4.3 完全推不出层级时

有些 PDF 全文一个字号（纯文本转的、或者机器生成的报告）。这时候：

```go
// 找不到任何标题候选时，不硬造层级 —— 直接返回无标题的纯文本，
// 让 MarkdownChunker 内部退化到 PlainChunker 走固定长度切分。
// 这条路径的 chunk 没有 heading_path，rag_query 会退回用 source 当出处
// （internal/agent/tools/rag_query.go:187 已有这个 fallback，不用改）。
//
// 宁可没有 heading_path，也不要错的 heading_path：
// 前者是信息缺失，后者是把用户导向错误的出处。
```

**已确认下游能承受**：`rag_query.go:187` 的注释写的是「优先用 heading_path」，
第 190 行是 `heading, _ := d.MetaData["heading_path"].(string)`，
取不到就是空串，已有降级分支。这条不用改代码。

---

## 5. 质量自检：抽取失败必须拒绝

鉴于 §2 列的那些 open issue，这一层是必需的。

```go
// checkQuality 在入库前拦截抽取失败的结果。
//
// 存在的理由很实际：ledongthuc/pdf 对 UniGB-UCS2-H（中文 PDF 最常见的
// Type0 编码）的支持是坏的（upstream issue #55 未修）。抽出来是乱码时
// 库不报错 —— 它以为自己成功了。
//
// 乱码入库的后果比入库失败严重得多：
//   - 入库失败：日志里一条 error，用户重传或换格式，5 分钟解决
//   - 乱码入库：向量库里多一批语义噪声，检索时偶尔飘出一段乱码，
//     排查时你会依次怀疑 embedding 模型、分块策略、RRF 权重……
//     最后才想到是三个月前入的某个 PDF
```

三道检查：

| 检查 | 阈值 | 拦的是什么 |
|---|---|---|
| 文本量 | 抽出字符 < 页数 × 50 | **扫描件**。整页是图，抽出来近乎空 |
| 可打印比例 | 非可打印字符（`unicode.IsGraphic` 为假，排除常规空白）> 5% | 编码错乱产生的控制字符 |
| 中文合理性 | 若原文声称中文却几乎无 CJK 码点，而拉丁字母异常多 | **正是 #55 的症状**：中文被解成拉丁乱码 |

第三条要说明清楚判据 —— 不能简单地「没中文就报错」，英文 PDF 是合法的。
判据是**字符分布异常**：真英文文档的字母组合符合英语词频，
而 #55 那种乱码（`l_n/9 6]ze/L5033`）标点和数字的比例会异常高。
实现上取一个保守指标：非字母数字的可见符号占比 > 30% 且无 CJK → 判为可疑。

**阈值全部可配**，因为它们必然需要在真实样本上调：

```toml
[parser.pdf]
min_chars_per_page  = 50
max_unprintable_pct = 5.0
```

---

## 6. 接入点

### 6.1 Ingester（改动最小）

`internal/pipeline/ingest.go` 的 `Run` 现在第一步就是 `chunker.Chunk`。
在它前面插一步：

```go
// 新增可选字段，nil 时行为与现在完全一致 —— 不影响任何现有调用方
type Ingester struct {
	parsers  parser.Registry // 新增
	chunker  rag.Chunker
	// ...
}
```

`Run` 的签名要动 —— 现在是 `Run(ctx, docID, raw)`，没有文件名就没法路由。
两个选择：

- **A**：加 `filename` 参数 → 改所有调用方（`ingest_check.go`、`consumer.go`、测试）
- **B**：加 `RunFile(ctx, docID, filename, raw)`，`Run` 保持不变内部调它

**选 B**。`Run` 是已经被测试和 Kafka consumer 依赖的稳定入口，
为一个可选功能改它的签名不划算。

### 6.2 Kafka ⚠️ 先问该不该用，再问怎么改

初版这一节直接在设计 claim-check 怎么改 Kafka。**顺序错了** ——
应该先确认 Kafka 该不该留。查了一遍调用方，情况和我以为的不一样。

#### 事实：Kafka 在生产路径上没有调用者

```
grep Publish / NewProducer / NewConsumer
  → 全部命中 internal/pipeline/kafka_integration_test.go

grep "internal/pipeline"
  → 只有 scripts/eval/ingest_check.go 一处

grep Kafka internal/app/app.go cmd/server/main.go
  → 无
```

**`internal/app` 和 `cmd/server` 完全没有引用 pipeline 包。**
路由表里只有 `/api/chat`、`/healthz`、`/metrics`、`/debug/pprof/` ——
**没有上传端点**。

也就是说：Kafka 管道写完了、测试齐全（含消费者重启续传、重复投递幂等），
但**没有任何生产代码调用它**。语料是 `ingest_corpus` 离线灌进去的，那条路不经过 Kafka。

Kafka 当初的立项理由是「上传接口收到文件立即返回，不阻塞 30 秒」——
**这个上传接口不存在。** 它解决的是一个尚未发生的问题。

#### 那么该用什么

先把需求分开。「异步入库」在这个项目里其实是两个独立需求：

| 需求 | 现状 | 真需要队列吗 |
|---|---|---|
| 批量灌语料（评估用） | `ingest_corpus` 同步跑，60 分钟超时，够用 | **不需要**。串行反而好观察进度、好中断 |
| 单文档上传后台处理 | **端点不存在** | 需要「不阻塞请求」，但不一定需要 Kafka |

对第二个需求，四个选项：

| 方案 | 运维成本 | 崩溃后任务是否还在 | 适配度 |
|---|---|---|---|
| **Kafka**（现状） | 高：broker + KRaft + 3 partition | ✅ | 杀鸡用牛刀 |
| **goroutine + channel** | 零 | ❌ 进程一死全丢 | 太弱 |
| **Redis Stream** | **零 —— Redis 已在跑** | ✅ 有 consumer group + ACK | **最贴合** |
| **PostgreSQL 任务表** | **零 —— PG 已在跑** | ✅ 且能事务性地和 chunks 一起提交 | **最贴合** |

后两个的共同优势：**不引入新组件**。Redis 和 PostgreSQL 已经是必需依赖
（缓存、BM25），用它们做队列等于零边际运维成本。而 Kafka 是**专门为这一个
未被调用的功能**引入的一整个 broker。

两者之间，对本项目 **PostgreSQL 任务表更合适**，理由是幂等性：

```sql
-- 入库任务表。为什么用 PG 而不是 Redis Stream：
-- 任务状态和 chunks 在同一个数据库，可以在一个事务里
-- 「写入 chunks + 标记任务完成」—— 不存在「chunk 写了但任务没标完成，
-- 重启后重复处理」的窗口。Redis Stream 的 ACK 和 PG 的写入是两个系统，
-- 这个窗口关不掉，只能靠 ChunkID 幂等去兜。
CREATE TABLE ingest_tasks (
    doc_id      VARCHAR(64) PRIMARY KEY,
    status      TEXT NOT NULL,   -- pending / running / done / failed
    ...
);
-- SELECT ... FOR UPDATE SKIP LOCKED 即可实现多 worker 竞争消费
```

`SKIP LOCKED` 是 PG 9.5 就有的特性，几十行代码就是一个可靠的任务队列。

#### 但我的建议是：现在什么都不改

尽管上面论证了 PG 更合适，**这次不要动它**：

1. **PDF 解析不需要队列**。走 `ingest_corpus` 离线入库，和现在灌 Markdown 一样。
   §8 的四步实施完全不碰 pipeline 包。
2. **删 Kafka 是独立决策**，牵涉 docker-compose、config、README、
   一整套集成测试，和 PDF 没有因果关系。混在一起做，出问题时分不清是谁的锅。
3. **Kafka 那套代码本身是有价值的**。集成测试覆盖了消费者重启续传、
   重复投递幂等、超限拒绝 —— 这些是能聊的东西。删掉等于把已完成的工作扔了。

所以结论是：**把「Kafka 是否过度设计」记进 BACKLOG 单独评估，
PDF 这条线绕开它。** 初版说的「claim-check 单独排期」方向是对的，
但理由说浅了 —— 不只是「工作量不小」，而是**这个改动可能根本不该做，
因为它服务的功能不存在**。

#### 面试角度

这件事本身比 Kafka 用得对不对更值得聊。被问到「为什么用 Kafka」时，
诚实的答法是：

> 立项时按「上传接口不能阻塞 30 秒」的场景引入的，管道和集成测试都完整。
> 但后来复盘发现上传端点一直没做，Kafka 在生产路径上没有调用者 ——
> 它解决的是一个还没发生的问题。如果重来，这个场景用 PostgreSQL 的
> `SELECT FOR UPDATE SKIP LOCKED` 就够了，PG 本来就在跑，
> 零新增运维成本，而且任务状态能和 chunks 在同一个事务里提交。

**「我引入了 X」远不如「我发现 X 是过度设计，并说得清替代方案」值钱。**
前者是会用工具，后者是有判断力。


### 6.3 ingest_corpus

`scripts/eval/ingest_corpus.go:81` 现在写死 `NewMarkdownChunker`，
`findMarkdown` 只找 `.md`。改成走 parser registry，扩展名放开到 `.md` / `.pdf`。

#### ⚠️ 6.3.1 docID 会碰撞 —— 静默丢数据

**这是本次评审查出的最严重问题，必须在放开扩展名之前修。**

`docIDFor`（`ingest_corpus.go:273`）会**剥掉扩展名**：

```go
rel = strings.TrimSuffix(filepath.ToSlash(rel), filepath.Ext(rel))
name := strings.ReplaceAll(rel, "/", "-")
```

已实测确认：

```
redis/guide.md   →  redis-guide
redis/guide.pdf  →  redis-guide     ← 同一个 docID
```

后果是**静默数据丢失**，且比看上去更糟：

1. `ChunkID = sha256(docID#index)` 两边完全一致
2. Milvus 用的是 **Upsert**（`milvus.go:179`），PG 也是 upsert
3. → **后入库的直接覆盖先入库的**，不报错、不警告

而且不会覆盖干净：如果 `guide.md` 切出 20 块、`guide.pdf` 切出 12 块，
覆盖后前 12 块是 PDF 的，**后 8 块是 Markdown 的残留** ——
向量库里躺着一篇内容拼接自两个文件的「缝合怪」文档。

**这正是上次那 346 行孤儿数据的同一类 bug。** BACKLOG 里记的原因是
「早期 docID 只用 basename，导致 en/ja 子目录同名文件互相覆盖」。
修法是改用相对路径 —— 但**扩展名当时没进 docID**，因为那时只有 `.md`，
不构成冲突。放开扩展名，这个坑立刻复活。

修法（一行）：

```go
// ⚠️ 扩展名必须进 docID。曾经只有 .md 时剥掉扩展名是安全的，
// 支持多格式后 guide.md 和 guide.pdf 会撞成同一个 docID，
// 而两个存储都是 upsert —— 后入库的静默覆盖先入库的，
// 且块数不同时会留下前一篇的残尾，拼成一篇不存在的文档。
// 这与 2026-08-20 清理的 346 行孤儿数据是同一类 bug。
name := strings.ReplaceAll(filepath.ToSlash(rel), "/", "-")  // 不再 TrimSuffix
```

注意 docID 有 60 字节上限（`maxBytes`），Milvus 侧 `maxIDBytes = 64`
且**只校验不截断**（`milvus.go:155` 注释明确写了「ID 截断会静默丢数据」）。
加上 `.pdf` 四个字节仍在预算内，但超长回退分支的哈希基准要一并确认用的是
带扩展名的 `rel`，否则哈希本身又碰撞了。

**必须加回归测试**，且按 gojieba 那次的标准 —— 把 bug 放回去要能变红：

```go
func TestDocIDFor_DistinguishesExtensions(t *testing.T) {
	// 少了这条，多格式支持会静默覆盖同名文档。
	// 验证方式：把 TrimSuffix 改回去，此测试必须失败。
	md := docIDFor("/c", "/c/redis/guide.md")
	pdf := docIDFor("/c", "/c/redis/guide.pdf")
	if md == pdf {
		t.Fatalf("docID collision: %q", md)
	}
}
```

#### ⚠️ 6.3.2 stripFrontmatter 不能喂给 PDF

`ingest_corpus.go:146` 与 `:220` 两处都无条件调用 `stripFrontmatter(raw)`。
它对 PDF 的原始字节做 `string(raw)` 再找 `---` —— PDF 是二进制，
理论上可能出现以 `---` 开头的字节序列而被误切。

概率很低（PDF 必须以 `%PDF-` 开头），但这一步**在语义上就不该发生**：
frontmatter 是 Markdown 的概念。

修法是把它移进 Markdown parser 内部，而不是留在调用方按格式判断 ——
调用方做格式分支，就等于把 registry 的意义抵消掉了。


---

## 7. 评估：这一节比实现更重要

### ⚠️ 不能污染现有基线

现在的 77 条 golden set 全部建立在 Markdown 语料上，
`docs/eval-results.md` 里的 MRR 0.798 / Recall@5 0.444 是**在这个语料上**测的。

**往语料库里加 PDF 会改变这些数字**，而且是双向的：
新文档既可能提供正确答案（提升召回），也可能成为干扰项（降低精度）。

所以：

1. **PDF 语料单独建 collection / 单独一套 golden set**，独立出数
2. 现有 77 条的评估**必须在原语料上重跑**并确认数字不变，才能继续引用
3. 两套数字**分开写**，不合并成一个「总体 MRR」——
   合并出来的数字没有可比性，也无法回答「PDF 支持得怎么样」

上次清理 346 行孤儿数据时就是这个教训：语料一动，所有已发布的数字全部失效，
当时重跑了评估并把旧结果留档在 `eval-results.before-cleanup.md`。
这次是**主动加数据**，更要提前把边界划清楚。

### PDF golden set 怎么建

现有的 77 条分布是 keyword 40 / semantic 29 / confusable 8。
PDF 集不用照抄这个分布，要针对 PDF 特有的失败模式设计：

| 类别 | 条数 | 验的是什么 |
|---|---|---|
| 层级正确 | 15 | 答案在某个子章节里，检查 `heading_path` 是否指对 |
| 跨页内容 | 8 | 答案正好跨页边界，检查有没有被页眉页脚切断 |
| 表格邻近 | 5 | 答案在表格附近的正文里（不要求解析表格，但不能被表格文本污染） |
| 中文抽取 | 10 | 不同字体编码的中文 PDF，**这类直接检验 #55 的影响面** |

#### ⚠️ 语料隔离需要改 eval.go，不是配置就能搞定

初版说「单独 collection / 单独 golden set」时没核对脚本支持不支持。核对结果：

```
scripts/eval/eval.go:65   -golden 是 flag ✅ 可以传 PDF golden set
scripts/eval/eval.go:102  collection 直接取 cfg.Milvus.Collection ❌ 写死
```

**golden set 能换，collection 不能换。** 所以「单独 collection」要么改 eval.go
加 `-collection` flag，要么临时改 `config.toml` 跑一遍再改回来 ——
后者是手工操作，容易忘记改回去，然后下次评估就在错的 collection 上跑。

PG 侧更麻烦：BM25 走的是固定的 `chunks` 表，隔离要么加 `corpus` 字段过滤，
要么建第二张表。**这是 §9 第②问的真实成本**，比初版暗示的高。

三种做法：

| 做法 | 成本 | 问题 |
|---|---|---|
| eval.go 加 `-collection` + PG 加 `corpus` 字段 | 中 | 最干净，但要动 migration |
| 混进现有语料重跑 | 低 | `eval-results.md` 历史对比断裂 |
| 另起一个 `config.pdf.toml` | 低 | PG 表还是同一张，隔离不彻底 |


### 独立的抽取质量测试（不依赖检索）

层级推断对不对，应该能脱离整条 RAG 链路单独测。
构造 PDF → 抽取 → 和手写的期望 Markdown 比对：

```
testdata/
  simple-3level.pdf       + .expected.md    标准三级标题
  no-heading.pdf          + .expected.md    全文单一字号 → 必须无标题降级
  with-header-footer.pdf  + .expected.md    页眉页脚必须被剔除
  cover-page.pdf          + .expected.md    封面不能独占 H1
  table.pdf               + .expected.md    表格行语义自洽，不与正文串行
  cjk-unigb.pdf                             ⚠️ 期望是「抽取失败」而非成功
```

最后一个是**反证测试**，和 gojieba 那次的思路一样：
如果 #55 的问题存在，这个用例必须**红**；哪天上游修了，它变绿，
说明可以放宽拒绝策略。**用测试把已知的上游缺陷钉住**，
比在文档里写一句「注意中文可能有问题」有用得多。

> 构造 testdata 需要能生成 PDF。`pdfcpu` 虽然不做文本抽取，
> 但可以用来**生成**测试样本（Apache-2.0，纯 Go），
> 只进 `testdata` 生成脚本不进主依赖。

---

## 8. 分步实施

拆成四个可独立验收的步骤。每步都能单独 merge，不存在「做到一半不能用」的状态。

| 步 | 内容 | 验收 | 估时 |
|---|---|---|---|
| **0** | **修 `docIDFor` 扩展名碰撞 + 回归测试**（§6.3.1） | **把 `TrimSuffix` 改回去，测试必须变红** | **0.5 天** |
| **1** | parser 包骨架 + 文本抽取 + 质量自检 | 能从 PDF 抽出纯文本；扫描件/乱码被拒绝；`cjk-unigb.pdf` 反证测试如实反映现状 | 1 天 |
| **1.5** | 表格 L1/L2：Rect 区域识别 + 线性化 | 表格文本不与正文串行；`table.pdf` 抽出的行语义自洽 | 1 天 |
| **2** | 字号聚类推断层级 | 6 个 testdata 全过；无标题文档正确降级 | 2 天 |
| **3** | 接入 parser registry + ingest_corpus | 目录里混放 md/pdf 能一起入库；`heading_path` 在检索结果里正确显示 | 1 天 |
| **4** | PDF golden set + 独立评估 | 出数，写进 `docs/eval-results-pdf.md`；**同时确认原 77 条数字未变** | 2 天 |

**步骤 0 是阻塞项**，必须最先做。它和 PDF 解析无关 —— 是现有代码在
「支持第二种扩展名」这个前提下就会暴露的 bug。**在放开扩展名之前修掉，
否则第一次混合入库就会静默污染语料**，而污染后重跑评估的数字全是废的
（上次 346 行孤儿数据已经演过一遍这个剧本）。

**步骤 1.5 必须排在 2 之前**：表格文本如果没被隔离出来，它会带着自己的字号
参与 §4 的字号统计，把正文字号的众数带偏 —— 层级推断的地基就是歪的。

**整条线不碰 `internal/pipeline`。** PDF 走 `ingest_corpus` 离线入库，
与现在灌 Markdown 的路径完全一致。Kafka 的存废（§6.2）是独立议题，
记进 BACKLOG 单独评估。

---

## 9. 需要你拍板的三件事

**① 抽取失败率高到什么程度就放弃这个库？**

考虑到 #55，我的建议是：**步骤 1 做完先拿 20 份真实中文 PDF 跑一遍**，
统计失败率。如果超过 30%，这个方案的价值就存疑了 —— 
到时候的选择是接受 AGPL 用 go-fitz，或者干脆维持现状不支持 PDF。

**先测再继续做，不要四步做完了才发现中文基本抽不出来。**

**② 要不要在 §7 的语料隔离上花这个成本？**

**核对后成本比初版说的高**：`eval.go:65` 的 `-golden` 是 flag（能换），
但 `:102` 的 collection 直接取 `cfg.Milvus.Collection`（写死），
PG 侧还共用同一张 `chunks` 表。彻底隔离要改 eval.go + 加 migration。
三种做法的取舍见 §7 那张表。


**③ Kafka 怎么处理？**（初版问的是「claim-check 现在做还是挂着」，
查完调用方后这个问题本身就问错了 —— 见 §6.2）

三个选项：

- **A（推荐）**：这次不动，把「Kafka 是否过度设计」记进 BACKLOG。
  PDF 走离线入库，不需要队列
- **B**：删掉 Kafka，改用 PG 任务表。但要连带做上传端点，否则还是没有调用者
- **C**：保留 Kafka 并补上传端点，让它真正跑起来

我倾向 **A**。B 和 C 都是在做「上传功能」，那是独立于 PDF 的另一件事。


---

## 附：改动清单

```
新增
  internal/rag/parser/parser.go          Parser 接口 + Registry
  internal/rag/parser/pdf.go             PDF → Markdown
  internal/rag/parser/outline.go         字号聚类（核心）
  internal/rag/parser/table.go           Rect 区域识别 + 线性化（L1/L2）
  internal/rag/parser/quality.go         抽取质量自检
  internal/rag/parser/testdata/          6 个样本 + 期望输出
  scripts/eval/golden_set_pdf.json       PDF 专用 golden set
  docs/eval-results-pdf.md               独立评估结果

修改
  scripts/eval/ingest_corpus.go          ⚠️ docIDFor 保留扩展名（阻塞项，§6.3.1）
                                         走 registry，扩展名放开
  internal/pipeline/ingest.go            +RunFile，Run 不动
  internal/config/config.go              +ParserConfig
  config.example.toml                    +[parser.pdf]
  go.mod                                 +github.com/ledongthuc/pdf

可能要改（取决于 §9 第②问怎么定）
  scripts/eval/eval.go                   +-collection flag（现在写死 cfg.Milvus.Collection）
  migrations/                            chunks 表 +corpus 字段用于语料隔离

不动
  internal/rag/types.go                  接口契约，不改
  internal/rag/chunker/*.go              完全复用
  internal/agent/tools/rag_query.go      sourceOf 已有三分支降级，已核对
  internal/pipeline/producer.go          ⚠️ 初版打算改，现在不改 —— 见 §6.2
  internal/pipeline/consumer.go          同上
  docker-compose.yml                     Kafka 存废是独立议题，不在本方案内
  pkg/vectorstore/milvus.go              content 上限 32KB 且已有截断，PDF 块撑不满
```

---

## 附二：本方案的三处自我修正

留档，因为修正的理由比结论有用。

**① 表格从「不做」改成「分三级做」**

初版理由「还原表格是独立课题」只对 L3 成立，我拿它把 L1/L2 一起砍了。
L1 不做会产生**错误数据**（表格文本串行污染 confusable 类查询），
L3 不做只是**功能缺失**。**把「缺功能」和「出错误」混为一谈，
是排优先级时最容易犯的错。**

**② Kafka 从「改 claim-check」改成「先问该不该留」**

初版直接跳到「怎么改」，跳过了「该不该用」。查调用方才发现：
`internal/app` 和 `cmd/server` 根本不引用 pipeline 包，
生产路径上**没有上传端点**，Kafka 只有集成测试在调。

它服务的是一个不存在的功能 —— 那么「PDF 太大装不进 Kafka 消息」
这个问题本身就是伪问题。**遇到「X 用起来有障碍」时，
先确认 X 是否在被使用，再去解决那个障碍。**

**③ 漏掉了 docID 碰撞（§6.3.1）—— 三处修正里最严重的**

前两处是判断偏差，这一处是**漏检**：初版把「扩展名放开到 .md/.pdf」
轻描淡写成一行改动，没去读 `docIDFor` 的实现。

而它会剥掉扩展名 → `guide.md` 和 `guide.pdf` 撞成同一个 docID
→ 两个存储都是 upsert → 静默覆盖 + 残尾拼接。**方案里最危险的一步，
初版写得最轻松。**

三者的共同教训：**「这一步很简单」这个判断本身，就是该去读代码的信号。**
前两处是没往下追一层，第三处是根本没打开文件。

---

## 附三：评审时逐条核对过的事实

避免下次重复查证。全部对照仓库代码确认，非推测：

| 断言 | 核对结果 |
|---|---|
| `sourceOf` 有 heading_path 缺失降级 | ✅ `rag_query.go:189-199`，四分支 switch |
| `heading_path` 写入位置 | ✅ `markdown.go:107` |
| Milvus content 上限 | ✅ 32KB（`maxContentBytes`），`milvus.go:173` 已做截断 |
| Milvus ID 上限 | ✅ 64 字节，**只校验不截断**，超长直接报错（`:155`） |
| docID 长度预算 | ✅ `maxBytes = 60`，加 `.pdf` 后仍在 64 以内 |
| `findMarkdown` 扩展名 | ✅ `:257` 只收 `.md` / `.mdx` |
| `docIDFor` 剥扩展名 | ⚠️ **确认会碰撞**，已实测复现 |
| `stripFrontmatter` 无条件调用 | ⚠️ `:146` 与 `:220` 两处 |
| eval.go golden set 可换 | ✅ `:65` 是 flag |
| eval.go collection 可换 | ❌ `:102` 写死 `cfg.Milvus.Collection` |
| Kafka 生产调用方 | ❌ **零个**，仅集成测试 |
| 上传端点存在 | ❌ 路由只有 chat/healthz/metrics/pprof |
| golden set 分布 | ✅ 77 条 = keyword 40 / semantic 29 / confusable 8 |
| `ledongthuc/pdf` 纯 Go + BSD-3 | ✅ pkg.go.dev |
| `Text` 结构体带 FontSize | ✅ pkg.go.dev API 文档 |
| `Content` 带 `Rect []Rect` | ✅ 同上，L1 表格识别的依据 |
| go-fitz AGPL-3.0 + cgo | ✅ pkg.go.dev，且预编译库不含 CJK 字体 |
| issue #55 未修 | ✅ open，PR #56 未合 |


