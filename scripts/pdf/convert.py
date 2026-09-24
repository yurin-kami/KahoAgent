#!/usr/bin/env python3
"""MinerU 输出 → 干净的 Markdown，供 ingest_corpus 直接入库。

    .venv/Scripts/python.exe convert.py <mineru输出目录> <markdown输出目录>

为什么读 content_list.json 而不是直接用 MinerU 产出的 .md：

  1. **层级是显式数字**。content_list.json 里每个块带 text_level
     （1=一级标题，2=二级标题，无该字段=正文），转 # 号无歧义。
     解析现成的 Markdown 等于把模型已判断好的结构降维成文本再重新解析。

  2. **能按 type 精确过滤**。页眉、页脚、页码、边注都有独立的 type，
     直接丢弃即可 —— 不需要靠 Y 坐标和跨页重复去猜。

  3. **表格能受控转换**。table 块的 table_body 是 HTML，
     由我们决定转成什么，而不是被动接受。

产物是普通 Markdown，MarkdownChunker 原封不动即可处理，
Go 侧零改动 —— 这是整个方案 B 的核心价值。
"""

import html as _html
import json
import re
import sys
from pathlib import Path

if hasattr(sys.stdout, "reconfigure"):
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")

# 排版元素，入库只会成为跨文档的重复噪声。
# 例：每页都有「第 3 章 Redis」的话，它会在几十个 chunk 里重复出现，
# 稀释真正的语义内容。
DROP_TYPES = {"header", "footer", "page_number", "aside_text", "page_footnote"}

# 本次不处理的类型（见 pdf-plan-b.md §2「明确不做」）。
# 图片和公式在当前语料里不存在，启用只会引入未经验证的失败模式。
DROP_TYPES |= {"image", "equation", "chart"}

MAX_LEVEL = 6  # Markdown 只有六级


def find_content_list(root: Path) -> list[Path]:
    """定位 content_list.json。

    ⚠️ 不写死文件名。官方文档只给了 {filename}_content_list.json 的命名
    模式，未给出完整文件清单，且明确写了产出随 backend 而变。
    实测 pipeline backend 的目录是 {name}/auto/ 而非文档暗示的 {backend}/。

    排除 _v2 变体：那是另一种结构，字段不同，混进来会静默产出错误结果。
    """
    out = [p for p in root.rglob("*_content_list.json") if not p.name.endswith("_v2.json")]
    if not out:
        raise SystemExit(
            f"未找到 *_content_list.json：{root}\n"
            f"请确认 MinerU 已成功运行（检查是否有模型下载失败）"
        )
    return sorted(out)


def table_to_md(body: str) -> str:
    """HTML 表格 → Markdown 表格。

    不保留 HTML：MarkdownChunker 不解析 HTML，原样入库会让 <td> 这类
    标签进入 embedding 文本，成为无语义的噪声 token。

    只做线性化不还原复杂结构（合并单元格等）—— 对 RAG 而言
    「这段文字和查询相关吗」才是问题，不是「第 3 行第 2 列是什么」。
    """
    rows = re.findall(r"<tr[^>]*>(.*?)</tr>", body, re.S | re.I)
    if not rows:
        return ""
    table: list[list[str]] = []
    for r in rows:
        cells = re.findall(r"<t[hd][^>]*>(.*?)</t[hd]>", r, re.S | re.I)
        # 单元格里可能嵌 <br>，转成空格而不是直接删，否则词会粘连
        cells = [re.sub(r"<br\s*/?>", " ", c, flags=re.I) for c in cells]
        cells = [_html.unescape(re.sub(r"<[^>]+>", "", c)).strip() for c in cells]
        cells = [re.sub(r"\s+", " ", c) for c in cells]
        if any(cells):
            table.append(cells)
    if not table:
        return ""

    width = max(len(r) for r in table)
    table = [r + [""] * (width - len(r)) for r in table]
    # 管道符必须转义，否则会把一列劈成两列
    esc = lambda c: c.replace("|", "\\|")  # noqa: E731
    lines = ["| " + " | ".join(esc(c) for c in table[0]) + " |",
             "|" + "---|" * width]
    lines += ["| " + " | ".join(esc(c) for c in r) + " |" for r in table[1:]]
    return "\n".join(lines)


def looks_like_heading(text: str) -> bool:
    """兜底判断：模型漏标的标题。

    实测 MinerU 的标题识别率约 85%（11/13），漏判的会被当成正文。
    漏一个标题的代价是该节所有内容并入上一节的 heading_path ——
    引用出处会指错，比没有出处更糟。

    判据保守：必须短、无句末标点，且匹配常见的中文小节编号格式。
    宁可漏判也不能误判 —— 把正文错当标题会把文档切碎。
    """
    t = text.strip()
    if len(t) > 30 or "\n" in t:
        return False
    if t.endswith(("。", "：", "，", "；", ".", "!", "?", "、")):
        return False
    return bool(re.match(r"^(?:[一二三四五六七八九十]+[、.]|\d+[、.]|第[一二三四五六七八九十\d]+[章节部])", t))


def convert(path: Path) -> tuple[str, dict]:
    blocks = json.loads(path.read_text(encoding="utf-8"))
    out: list[str] = []
    stats = {"text": 0, "heading": 0, "rescued": 0, "table": 0, "code": 0, "dropped": 0}

    for b in blocks:
        btype = b.get("type", "text")
        if btype in DROP_TYPES:
            stats["dropped"] += 1
            continue

        if btype == "table":
            md = table_to_md(b.get("table_body", ""))
            if md:
                cap = " ".join(b.get("table_caption") or []).strip()
                if cap:
                    out.append(cap)
                out.append(md)
                stats["table"] += 1
            continue

        if btype == "code":
            # ⚠️ 字段是 code_body 不是 text，且 MinerU 已经带上了
            # ```lang 围栏 —— 实测确认过，不要凭 type 名猜字段名。
            code = str(b.get("code_body", "")).strip()
            if code:
                cap = " ".join(b.get("code_caption") or []).strip()
                if cap:
                    out.append(cap)
                if code.startswith("```"):
                    # 已自带围栏：只need确保闭合。MarkdownChunker 会跟踪
                    # 围栏状态，不闭合会让后续内容全被当成代码块而不切分。
                    if code.count("```") % 2 == 1:
                        code += "\n```"
                    out.append(code)
                else:
                    fence = "````" if "```" in code else "```"
                    out.append(f"{fence}\n{code}\n{fence}")
                stats["code"] += 1
            continue

        text = str(b.get("text", "")).strip()
        if not text:
            continue

        level = b.get("text_level") or 0
        if not level and looks_like_heading(text):
            level = 3  # 兜底出来的一律降一级，避免抢了真标题的层级
            stats["rescued"] += 1

        if level:
            out.append("#" * min(int(level), MAX_LEVEL) + " " + text)
            stats["heading"] += 1
        else:
            out.append(text)
            stats["text"] += 1

    return "\n\n".join(out) + "\n", stats


def main() -> int:
    if len(sys.argv) != 3:
        print(__doc__)
        return 1
    src, dst = Path(sys.argv[1]), Path(sys.argv[2])
    dst.mkdir(parents=True, exist_ok=True)

    files = find_content_list(src)
    print(f"发现 {len(files)} 份 MinerU 产物\n")

    total = 0
    for f in files:
        name = f.name.replace("_content_list.json", "")
        md, st = convert(f)

        # ⚠️ 文件名加 pdf- 前缀。两个作用：
        #  1. docID 天然带前缀，与 Markdown 语料不会碰撞
        #  2. 入库后可用 doc_id LIKE 'pdf-%' 区分两类语料
        target = dst / f"pdf-{name}.md"
        target.write_text(md, encoding="utf-8")
        total += 1
        print(f"  {target.name:52} 标题{st['heading']:3}"
              f"(兜底{st['rescued']}) 正文{st['text']:3}"
              f" 表{st['table']} 码{st['code']} 丢{st['dropped']}")

    print(f"\n[OK] 转换 {total} 份 → {dst}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
