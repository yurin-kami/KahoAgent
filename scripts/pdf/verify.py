#!/usr/bin/env python3
"""转换质量验证：把 MinerU 产物与源 Markdown 逐份比对并给出量化指标。

    .venv/Scripts/python.exe verify.py <源md目录> <converted目录>

为什么需要这个脚本：
「转出来看着还行」不是验收标准。PDF 转换的失败模式很多是**静默**的 ——
标题漏判会让整节内容并入上一节（引用出处指错），
中文抽取失败会产出乱码（污染向量空间），
两者都不会报错。必须有可量化的判据。

四项指标，每项都对应一个真实的失败模式：

  标题召回率  漏判 → heading_path 指错，比没有出处更糟
  中文占比    骤降 → 编码失败，抽出来是乱码
  内容留存率  骤降 → 大段内容丢失（分栏、图片化文本）
  方块字      出现 → 字体缺字形，内容不可读

阈值不是拍脑袋定的，见 THRESHOLDS 处的说明。
"""

import re
import sys
from pathlib import Path

if hasattr(sys.stdout, "reconfigure"):
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")

# 判定阈值。
#
# 标题召回 0.70：实测单份样本是 0.85（11/13），版面模型有固有误差，
#   留出余量。低于 0.70 说明层级结构基本没识别出来，heading_path 不可信。
# 中文占比比值 0.80：转换后中文字符占比不应比源文低太多。
#   骤降意味着编码失败 —— 这是 pdf-plan.md 里那个 UniGB-UCS2-H 问题的通用检测。
# 内容留存 0.60：分块和排版会损失一些文本，但不该少于六成。
#   注：源文含 Markdown 标记（**、`、|），转换后部分被吃掉，故不能要求接近 1.0。
THRESHOLDS = {"heading_recall": 0.70, "cjk_ratio": 0.80, "retention": 0.60}

# 不适合参与失败率统计的样本判据。
#
# ⚠️ 这不是「把失败样本排除掉让数字好看」。每一条对应的都是**源文档本身
# 没有可抽取的正文**，PDF 转换在这些样本上没有可评判的对象：
#   - 主体是图片的文章（施密特正交化那篇，正文就一句话 + 一张图）
#   - 主体是代码块的文章（大纲测试那篇，11921 字里绝大部分是 C++ 源码）
#   - 全英文技术日志（崩溃转储，cjk_ratio 0.22 是内容如此，不是抽取失败）
# 把它们计入失败率，衡量的是「博客里有没有图片文章」而非「转换质量」。
#
# 用阈值自动判定而不是硬编码文件名 —— 后者换一批语料就失效了。
MIN_TEXT_CHARS = 400  # 源文可比正文少于此数：样本太小，单个标题就能让指标剧烈波动
MIN_CJK_CHARS = 200  # 中文太少：cjk_ratio 失去意义

CJK = re.compile(r"[一-鿿]")
BLOCK = re.compile(r"[■�]")  # ■ 和 U+FFFD 替换字符


def norm(s: str) -> str:
    return re.sub(r"\s+", "", s)


def headings(md: str) -> list[str]:
    return [norm(m.group(2)) for m in re.finditer(r"^(#{1,6})\s+(.+)$", md, re.M)]


def strip_frontmatter(md: str) -> tuple[str, str]:
    """返回 (正文, title)。与 ingest_corpus 的行为一致。"""
    if not md.startswith("---"):
        return md, ""
    rest = md[3:]
    end = rest.find("\n---")
    if end < 0:
        return md, ""
    title = ""
    for line in rest[:end].split("\n"):
        if line.strip().startswith("title:"):
            title = line.split(":", 1)[1].strip().strip("\"'")
            break
    return rest[end + 4:].lstrip("\r\n"), title


def text_only(md: str) -> str:
    """剥掉 Markdown 标记与不可抽取的元素，只留可比较的文字。

    ⚠️ 必须剥掉代码块和图片，否则留存率会失真：
      - 代码块：源文里可能占绝大部分体积（实测有篇 11921 字的文章
        绝大部分是 C++ 源码），计入分母会让留存率虚低
      - 图片：PDF 里是图，根本没有文本可抽，计入分母是在惩罚
        「文档里有图」这件事
    """
    md = re.sub(r"```.*?```", "", md, flags=re.S)
    md = re.sub(r"!\[[^\]]*\]\([^)]*\)", "", md)  # 图片
    md = re.sub(r"^#{1,6}\s+", "", md, flags=re.M)
    md = re.sub(r"[*`>|\-#\[\]()]", "", md)
    return norm(md)


def check(src: Path, got: Path) -> dict:
    src_raw = src.read_text(encoding="utf-8")
    body, title = strip_frontmatter(src_raw)
    out = got.read_text(encoding="utf-8")

    want = ([norm(title)] if title else []) + headings(body)
    have = headings(out)
    hit = sum(1 for w in want if any(w in h or h in w for h in have))

    s_txt, o_txt = text_only(body), text_only(out)
    s_cjk_n = len(CJK.findall(s_txt))
    s_cjk = s_cjk_n / max(len(s_txt), 1)
    o_cjk = len(CJK.findall(o_txt)) / max(len(o_txt), 1)

    # 样本是否适合参与统计 —— 判据见文件头的常量说明
    skip = ""
    if len(s_txt) < MIN_TEXT_CHARS:
        skip = f"正文仅{len(s_txt)}字"
    elif s_cjk_n < MIN_CJK_CHARS:
        skip = f"中文仅{s_cjk_n}字"

    return {
        "heading_recall": hit / max(len(want), 1),
        "headings": f"{hit}/{len(want)}",
        "cjk_ratio": o_cjk / max(s_cjk, 1e-9),
        "retention": len(o_txt) / max(len(s_txt), 1),
        "blocks": len(BLOCK.findall(out)),
        "skip": skip,
    }


def main() -> int:
    if len(sys.argv) != 3:
        print(__doc__)
        return 1
    src_dir, got_dir = Path(sys.argv[1]), Path(sys.argv[2])

    results, skipped, failed = [], [], []
    for got in sorted(got_dir.glob("pdf-*.md")):
        stem = got.stem[4:]  # 去掉 pdf- 前缀
        src = src_dir / f"{stem}.md"
        if not src.exists():
            print(f"[SKIP] 找不到源文件 {src.name}")
            continue

        r = check(src, got)
        r["name"] = stem
        if r["skip"]:
            skipped.append(r)
            continue

        bad = [k for k, v in THRESHOLDS.items() if r[k] < v] + (
            ["blocks"] if r["blocks"] else []
        )
        r["ok"] = not bad
        r["bad"] = bad
        results.append(r)
        if bad:
            failed.append(r)

    if not results:
        print("没有可验证的文件")
        return 1

    print(f"{'文档':<46}{'标题':>8}{'中文':>8}{'留存':>8}{'方块':>6}  判定")
    print("-" * 84)
    for r in results:
        print(f"{r['name'][:44]:<46}{r['headings']:>8}"
              f"{r['cjk_ratio']:>8.2f}{r['retention']:>8.2f}{r['blocks']:>6}"
              f"  {'OK' if r['ok'] else 'FAIL:' + ','.join(r['bad'])}")

    if skipped:
        # ⚠️ 必须打出来。静默排除样本，读者会以为覆盖了全部文档 ——
        # 那和悄悄把阈值调松没有区别。
        print(f"\n不计入统计（源文档无可抽取正文）：")
        for r in skipped:
            print(f"  {r['name'][:52]:<54} {r['skip']}")

    n, f = len(results), len(failed)
    rate = f / n
    print("-" * 84)
    print(f"合计 {n} 份（另有 {len(skipped)} 份不计入），失败 {f} 份，失败率 {rate:.1%}")
    print(f"平均标题召回 {sum(r['heading_recall'] for r in results)/n:.1%}"
          f" · 平均内容留存 {sum(r['retention'] for r in results)/n:.1%}")

    # 30% 是 pdf-plan-b.md §7 写死的停止条件：超过就该换 Docling 重新评估
    if rate >= 0.30:
        print(f"\n[STOP] 失败率 {rate:.1%} ≥ 30%，触发 pdf-plan-b.md §7 停止条件")
        return 2
    print(f"\n[OK] 失败率 {rate:.1%} < 30%，通过")
    return 0


if __name__ == "__main__":
    sys.exit(main())
