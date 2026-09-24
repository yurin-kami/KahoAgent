#!/usr/bin/env python3
"""从 Markdown 生成测试用 PDF（供 MinerU 转换质量验证）。

    .venv/Scripts/python.exe make_testpdf.py <input.md> <output.pdf>

为什么要自己造 PDF 而不是找现成的：
验证 MinerU 的转换质量需要「已知正确答案」——只有确切知道原文的
标题层级、表格内容、代码块边界，才能判断转换结果对不对。
拿一份来源不明的 PDF，转出来是好是坏根本无从判断。

用博客语料生成还有一层好处：内容与现有 77 篇语料同源，都是中文技术
文档，排版特征一致，转换结果更有代表性。

用 reportlab 而不是 Chrome/weasyprint：
  - Chrome headless 在本机 --print-to-pdf 静默失败（退出码 0 但不产出文件），
    且访问 %TEMP% 下的 file:// 会被拦，生成的是「ERR_FILE_NOT_FOUND」错误页
  - weasyprint 缺 GTK 运行库
  - reportlab 已是 MinerU 的依赖，装在同一个 venv 里，零新增安装
"""

import re
import sys
from pathlib import Path

from reportlab.lib.enums import TA_JUSTIFY
from reportlab.lib.pagesizes import A4
from reportlab.lib.styles import ParagraphStyle, getSampleStyleSheet
from reportlab.lib.units import mm
from reportlab.pdfbase import pdfmetrics
from reportlab.pdfbase.cidfonts import UnicodeCIDFont
from reportlab.platypus import (
    Paragraph,
    Preformatted,
    SimpleDocTemplate,
    Spacer,
    Table,
    TableStyle,
)
from reportlab.lib import colors

if hasattr(sys.stdout, "reconfigure"):
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")

# CID 字体是 reportlab 内置的中日韩支持，不需要外部字体文件。
# 不用 TTF 是因为字体路径在不同机器上不一样，会让脚本不可移植。
CN_FONT = "STSong-Light"
pdfmetrics.registerFont(UnicodeCIDFont(CN_FONT))

# 标题字号必须逐级拉开：MinerU 靠版面模型识别层级，
# 字号相近的标题人眼都难分，不该要求模型分出。
#
# ⚠️ 必须覆盖到 h6。只定义到 h3 的话，源文里的 #### 会当成正文渲染，
# PDF 里根本没有字号差异 —— 模型无从识别。实测这一项让标题召回
# 从 80% 掉到 56%，而这是**测试数据的缺陷**不是 MinerU 的问题，
# 差点据此误判要换库。
_base = getSampleStyleSheet()


def _h(size: float, before: int, after: int) -> ParagraphStyle:
    return ParagraphStyle(
        f"h{size}", parent=_base["Heading1"], fontName=CN_FONT,
        fontSize=size, leading=size * 1.3,
        spaceBefore=before, spaceAfter=after,
    )


S = {
    "h1": _h(24, 0, 14),
    "h2": _h(18, 18, 9),
    "h3": _h(14.5, 14, 7),
    "h4": _h(12, 11, 6),
    "h5": _h(10.5, 9, 5),
    "h6": _h(9.5, 8, 4),
    "p": ParagraphStyle("p", parent=_base["BodyText"], fontName=CN_FONT,
                        fontSize=8.5, leading=14, alignment=TA_JUSTIFY,
                        spaceAfter=7),
    "code": ParagraphStyle("code", parent=_base["Code"], fontName=CN_FONT,
                           fontSize=8, leading=10.5),
    "cell": ParagraphStyle("cell", parent=_base["BodyText"], fontName=CN_FONT,
                           fontSize=8.5, leading=12),
}


def strip_frontmatter(md: str) -> str:
    """剥掉 YAML frontmatter，把 title 提成 H1。

    与 ingest_corpus.go 的 stripFrontmatter 行为保持一致，
    否则生成的 PDF 结构与 Markdown 语料不可比。
    """
    if not md.startswith("---"):
        return md
    rest = md[3:]
    end = rest.find("\n---")
    if end < 0:
        return md
    front, body = rest[:end], rest[end + 4:]
    title = ""
    for line in front.split("\n"):
        if line.strip().startswith("title:"):
            title = line.split(":", 1)[1].strip().strip("\"'")
            break
    return (f"# {title}\n\n" if title else "") + body.lstrip("\r\n")


def inline(text: str) -> str:
    """行内标记 → reportlab 的 mini-HTML。

    ⚠️ 不用 Courier 渲染行内代码：reportlab 的 Courier 不含中文字形，
    中文注释会渲染成 ■■■ 方块，转出来的 Markdown 里就是一串黑块。
    实测踩过 —— 代码块里的中文注释全变成了方块。
    """
    text = (text.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;"))
    text = re.sub(r"\*\*(.+?)\*\*", r"<b>\1</b>", text)
    text = re.sub(r"`(.+?)`", r"<b>\1</b>", text)
    return text


def build(md: str) -> list:
    story: list = []
    rows: list[str] = []
    fence: list[str] | None = None

    def flush_table() -> None:
        if not rows:
            return
        cells = [[c.strip() for c in r.strip().strip("|").split("|")] for r in rows]
        cells = [c for c in cells if not all(set(x) <= set("-: ") for x in c)]
        rows.clear()
        if not cells:
            return
        data = [[Paragraph(inline(c), S["cell"]) for c in row] for row in cells]
        t = Table(data, hAlign="LEFT")
        t.setStyle(TableStyle([
            ("GRID", (0, 0), (-1, -1), 0.6, colors.black),
            ("BACKGROUND", (0, 0), (-1, 0), colors.Color(0.93, 0.93, 0.93)),
            ("VALIGN", (0, 0), (-1, -1), "TOP"),
            ("LEFTPADDING", (0, 0), (-1, -1), 5),
            ("RIGHTPADDING", (0, 0), (-1, -1), 5),
        ]))
        story.extend([t, Spacer(1, 8)])

    for line in md.split("\n"):
        # 代码围栏优先：块内的 # 和 | 都不是标记
        if line.strip().startswith("```"):
            if fence is None:
                flush_table()
                fence = []
            else:
                story.extend([Preformatted("\n".join(fence), S["code"]), Spacer(1, 6)])
                fence = None
            continue
        if fence is not None:
            fence.append(line)
            continue

        if line.strip().startswith("|"):
            rows.append(line)
            continue
        flush_table()

        if m := re.match(r"^(#{1,6})\s+(.+)", line):
            story.append(Paragraph(inline(m.group(2).strip()),
                                   S[f"h{len(m.group(1))}"]))
        elif line.strip():
            story.append(Paragraph(inline(line.strip()), S["p"]))

    flush_table()
    if fence is not None:  # 未闭合的围栏也要落盘，否则内容凭空消失
        story.append(Preformatted("\n".join(fence), S["code"]))
    return story


def main() -> int:
    if len(sys.argv) != 3:
        print(__doc__)
        return 1
    src, dst = Path(sys.argv[1]), Path(sys.argv[2]).resolve()

    md = strip_frontmatter(src.read_text(encoding="utf-8"))
    dst.parent.mkdir(parents=True, exist_ok=True)

    SimpleDocTemplate(
        str(dst), pagesize=A4,
        leftMargin=18 * mm, rightMargin=18 * mm,
        topMargin=20 * mm, bottomMargin=20 * mm,
        title=src.stem,
    ).build(build(md))

    if not dst.exists():
        print(f"[FAIL] 未生成 {dst}")
        return 1

    # 生成成功不等于内容正确 —— 之前用 Chrome 时踩过：
    # 退出码 0、文件也在，内容却是一张错误页。
    kb = dst.stat().st_size // 1024
    if kb < 5:
        print(f"[WARN] {dst.name} 仅 {kb} KB，内容可能不完整，请打开确认")
    print(f"[OK] {dst.name}  {kb} KB")
    return 0


if __name__ == "__main__":
    sys.exit(main())
