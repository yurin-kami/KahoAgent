# PDF 离线预处理

把 PDF 转成 Markdown，喂给现有的 `ingest_corpus` 入库。

**PDF 处理不进 Go 代码、不进镜像、不进 CI。** 入库是离线一次性动作
（`ingest_corpus` 带 `corpus` build tag，不在生产请求路径上），
没必要为它引入运行时依赖。完整推理见 [`../../docs/pdf-plan-b.md`](../../docs/pdf-plan-b.md)。

```
PDF ──[MinerU]──▶ content_list.json ──[convert.py]──▶ .md ──▶ ingest_corpus
```

## 安装

```bash
python -m venv scripts/pdf/.venv
scripts/pdf/.venv/Scripts/python.exe -m pip install "mineru[core]"
```

`.venv/` 已在 `.gitignore` 里 —— 模型和依赖合计数 GB，不入库。

## 用法

```bash
# 0. 环境变量（Windows 必须，见下文「已知问题」）
export PYTHONIOENCODING=utf-8 PYTHONUTF8=1
export MINERU_MODEL_SOURCE=modelscope        # 国内网络必须

# 1. PDF → MinerU 中间产物
.venv/Scripts/mineru.exe -p <pdf目录> -o out -b pipeline -l ch

# 2. 中间产物 → 干净 Markdown
.venv/Scripts/python.exe convert.py out converted

# 3. 质量验证（有源 Markdown 可比对时）
.venv/Scripts/python.exe verify.py <源md目录> converted
.venv/Scripts/python.exe verify_tables.py <源md目录> converted   # 表格单独验，见下

# 4. 入库（回到项目根目录，用独立配置隔离语料）
cp config.pdf.toml.example config.pdf.toml
podman exec kaho-postgres createdb -U kaho -O kaho kahoagent_pdf
KAHO_CONFIG=config.pdf.toml go run -tags=corpus ./scripts/eval/ingest_corpus.go scripts/pdf/converted
```

⚠️ **入库必须用 `KAHO_CONFIG` 指向独立配置**，否则会灌进主语料的
collection —— 而 Milvus 是 upsert，灌错了不报错，只能 Drop 整个
collection 重灌。评估结果与隔离设计见
[`../../docs/eval-results-pdf.md`](../../docs/eval-results-pdf.md)。

### MinerU 参数说明

| 参数 | 值 | 理由 |
|---|---|---|
| `-b` | `pipeline` | **CPU 可跑**。默认 `hybrid-engine` 要 GPU |
| `-l` | `ch` | 中文语料，明确指定比自动检测稳 |
| `-t` | 默认开启 | **表格要的** —— 这正是方案 B 优于 Go 库的地方 |

## LICENSE 结论

MinerU 3.4.5 用的是 `LicenseRef-MinerU-Open-Source-License`
（**基于 Apache-2.0 的自定义许可**，不是标准 Apache-2.0）。
三条附加条款逐条核对：

| 条款 | 触发条件 | 本项目 |
|---|---|---|
| 1. 商业许可门槛 | MAU > 1 亿 **或** 月营收 > 2000 万美元 | 不触发 |
| 2. 在线服务标注 | **基于 MinerU 对第三方提供在线服务**时须标注 | 不触发（离线用，不进服务）|
| 3. 终止 | 违反 1 或 2 | 不适用 |

**对本用途无碍。** 第 2 条顺带印证了方案 B 的价值：
若把 PDF 解析编进服务（方案 A 的形态），一旦对外提供就得在界面标注；
离线用完全没有这个义务。

许可证原文：`.venv/Lib/site-packages/mineru-3.4.5.dist-info/licenses/LICENSE.md`

## 已知问题（都实测踩过）

**① 必须设 `MINERU_MODEL_SOURCE=modelscope`**

默认从 huggingface.co 下模型，国内网络会 `LocalEntryNotFoundError`。
首次运行要下约 1GB 模型，耗时几分钟；之后走本地缓存。

**② Windows 必须设 `PYTHONIOENCODING=utf-8`**

Python 在 Windows 默认按 GBK 写 stdout，打印中文或 emoji 会
`UnicodeEncodeError` —— 而且**异常本身会盖掉真正的报错信息**，极难排查。
建议直接加进 PowerShell profile。

**③ MinerU 的字段名不能靠 type 猜**

实测确认（`content_list.json`）：

| type | 内容字段 | 备注 |
|---|---|---|
| `text` | `text` | `text_level`：1=H1，2=H2，无=正文 |
| `code` | **`code_body`** | 已自带 ` ```lang ` 围栏 |
| `table` | **`table_body`** | HTML 格式 |

一度按 `text` 去读 code 块，结果 8 个代码块全部丢失且不报错。

**④ 输出目录是 `{name}/auto/`**

官方文档暗示是 `{name}/{backend}/`，实测 pipeline backend 落在 `auto/`。
所以 `convert.py` 用 `rglob("*_content_list.json")` 定位而非拼路径，
并排除 `_v2` 变体（那是另一种结构，字段不同）。

**⑤ 标题识别率约 85%，不是 100%**

实测单份样本 11/13。漏判的标题会被当成正文，
后果是该节内容并入上一节的 `heading_path` —— **引用出处指错比没有出处更糟**。

`convert.py` 有一层保守兜底（`looks_like_heading`）：短、无句末标点、
且匹配中文小节编号格式（`一、` `1.` `第三章`）的才救。
宁可漏判也不误判 —— 把正文错当标题会把文档切碎。

## 测试数据生成

`make_testpdf.py` 从博客 Markdown 生成 PDF，用于验证转换质量。

**为什么要自己造 PDF**：验证转换质量需要「已知正确答案」——
只有确切知道原文的标题层级、表格内容、代码块边界，才能判断转换对不对。
拿一份来源不明的 PDF，转出来是好是坏根本无从判断。

用 `reportlab`（MinerU 的依赖，零新增安装），不用 Chrome 或 weasyprint：

- **Chrome headless** 在本机 `--print-to-pdf` 静默失败：退出码 0、
  但不产出文件；且访问 `%TEMP%` 下的 `file://` 会被拦，
  生成的是一张「ERR_FILE_NOT_FOUND」错误页 ——
  文件存在、退出码正常，只有打开看内容才会发现。
  一度以为是 MinerU 的问题（转出来只有 4 个块）。
- **weasyprint** 缺 GTK 运行库。

⚠️ 代码块也要用中文字体（`STSong-Light`）。reportlab 的 `Courier`
不含中文字形，代码里的中文注释会渲染成 `■■■` 方块。

## 文件

```
make_testpdf.py   Markdown → PDF（造测试数据）
convert.py        MinerU content_list.json → 干净 Markdown
verify.py         转换质量量化验证（4 项指标 + 30% 失败率阈值）
verify_tables.py  表格保真单独验证（单元格级比对）
testdata/         生成的测试 PDF
out/              MinerU 中间产物
converted/        最终 Markdown（文件名带 pdf- 前缀）
```

**表格为什么要单独验**：`verify.py` 的四项指标没有一项能发现表格问题 ——
表格整个丢掉，「内容留存」只掉几个百分点，照样过阈值。
而表格恰恰是选 MinerU 而非 Go 库的主要理由，不验等于核心卖点没验收。
实测 15 个表格零丢失、单元格保真 99.7%。

`pdf-` 前缀有两个作用：docID 与 Markdown 语料不碰撞，
且入库后可用 `doc_id LIKE 'pdf-%'` 区分两类语料。
