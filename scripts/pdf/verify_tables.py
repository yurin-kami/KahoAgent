#!/usr/bin/env python3
"""表格保真比对：源 Markdown 的表格内容有没有在转换产物里完整保留。

    .venv/Scripts/python.exe verify_tables.py <源md目录> <converted目录>

为什么单独一个脚本而不是塞进 verify.py：
`verify.py` 的四项指标（标题召回/中文占比/内容留存/方块字）**没有一项
能发现表格问题**。表格整个丢掉，「内容留存」只掉几个百分点，照样过阈值；
而表格恰恰是选 MinerU 而非 Go 库的主要理由（见 pdf-plan-b.md §1），
不验等于核心卖点没验收。

比对方式是**单元格级的集合包含**，不是逐行比对：
转换会改变行序、合并空行、调整对齐，逐行比会全是假阳性。
只问「源文里这个格子的内容，在产物的表格里还找得到吗」。

⚠️ 已知假阳性：ASCII 时序图。形如
    | (选定算法+KeyShare)----->  |
的行以 `|` 开头，会被当成表格行。实测 http-evolution 那篇因此
误报 3 个「丢失」。所以脚本会把疑似图形的格子单独列出而不计入损失 ——
判据是「去掉标点后没有任何中文或字母数字」，宁可漏报也不误报。
"""

import os
import re
import sys
from pathlib import Path

if hasattr(sys.stdout, "reconfigure"):
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")

# 分隔行（|---|:--:|）不是内容，不参与比对
SEP = set("-: ")
# 疑似 ASCII 时序图的格子：含连续 3 个以上的 - 或 = 画线，
# 或以 <-- / --> 箭头收尾。
#
# ⚠️ 判据不能写成「去掉标点后什么都不剩」—— 实测那样识别不出来，
# 因为 `(选定算法+KeyShare)----->` 里既有中文又有字母，
# 剥掉符号后残留 `选定算法KeyShare`，看起来像正常内容。
# 真正的特征是**画线**：表格单元格里不会出现连续的 --- 或 ---> 。
ART = re.compile(r"-{3,}|={3,}|<-{2,}|-{2,}>")


def cells(text: str) -> list[str]:
    """抽出所有表格单元格，归一化空白与强调标记。

    归一化是必须的：MinerU 输出的表格里 `**加粗**` 可能变成纯文本，
    空格数量也常有出入 —— 不归一化的话这些都会算成「内容丢失」。
    """
    out = []
    for line in text.split("\n"):
        line = line.strip()
        if not line.startswith("|"):
            continue
        row = [c.strip() for c in line.strip("|").split("|")]
        if all(set(c) <= SEP for c in row):
            continue
        out += [re.sub(r"[\s*`]", "", c) for c in row if c.strip()]
    return out


def count_tables(text: str) -> int:
    """连续两行以上的 | 开头的行算一个表格。"""
    n, run = 0, 0
    for line in text.split("\n"):
        if line.strip().startswith("|"):
            run += 1
        else:
            if run >= 2:
                n += 1
            run = 0
    return n + (1 if run >= 2 else 0)


def main() -> int:
    if len(sys.argv) != 3:
        print(__doc__)
        return 1
    src_dir, got_dir = Path(sys.argv[1]), Path(sys.argv[2])

    tot = hit = t_src = t_got = 0
    lost: list[tuple[str, str]] = []
    art: list[tuple[str, str]] = []

    for got in sorted(got_dir.glob("pdf-*.md")):
        src = src_dir / got.name[4:]  # 去掉 pdf- 前缀
        if not src.exists():
            continue
        s_raw, g_raw = src.read_text(encoding="utf-8"), got.read_text(encoding="utf-8")
        s_cells = cells(s_raw)
        if not s_cells:
            continue

        g_cells = set(cells(g_raw))
        t_src += count_tables(s_raw)
        t_got += count_tables(g_raw)

        n_lost = 0
        for c in s_cells:
            if c in g_cells:
                hit += 1
            elif ART.search(c):
                art.append((src.name, c))  # ASCII 图形，不计损失
            else:
                lost.append((src.name, c))
                n_lost += 1
        tot += len(s_cells)

        mark = "OK" if not n_lost else f"丢 {n_lost}"
        print(f"{src.stem[:52]:<54} {len(s_cells) - n_lost:>4}/{len(s_cells):<4} {mark}")

    if not tot:
        print("没有找到含表格的可比对文件")
        return 1

    if art:
        # 必须打出来 —— 静默排除样本和悄悄放宽阈值没有区别
        print(f"\n不计入（疑似 ASCII 图形，非真实表格）{len(art)} 个：")
        for name, c in art[:10]:
            print(f"  {name[:44]:<46} {c[:44]!r}")

    if lost:
        print(f"\n真实丢失 {len(lost)} 个单元格：")
        for name, c in lost[:20]:
            print(f"  {name[:44]:<46} {c[:44]!r}")

    real = tot - len(art)
    print("-" * 74)
    print(f"表格数量：源 {t_src} → 产物 {t_got}")
    print(f"单元格保真：{hit}/{real} = {hit / max(real, 1):.1%}"
          f"（另有 {len(art)} 个 ASCII 图形不计入）")

    # ⚠️ 这是内容保真，不是结构保真。内容都在、行列错位的话，
    # 这个数字仍然是 100% —— 已在 eval-results-pdf.md 的「已知局限」里记录。
    return 0 if hit == real and t_src == t_got else 2


if __name__ == "__main__":
    sys.exit(main())
