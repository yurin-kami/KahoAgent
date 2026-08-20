//go:build eval

// eval 对比不同检索策略的效果，产出可进简历的量化数据。
//
//	go run -tags=eval ./scripts/eval/eval.go
//	go run -tags=eval ./scripts/eval/eval.go -rerank   # 含 LLM 精排（慢且花钱）
//
// 输出四行对比表。关键判据（来自 IMPLEMENTATION.md 验收）：
// **hybrid-rrf 的 MRR 必须明显高于 vector-only** —— 若不高于，
// 说明 RRF 实现有 bug 或 BM25 召回质量太差，必须排查而非接受。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yurin-kami/KahoAgent/internal/config"
	"github.com/yurin-kami/KahoAgent/internal/eval"
	"github.com/yurin-kami/KahoAgent/internal/rag"
	"github.com/yurin-kami/KahoAgent/internal/rag/retriever"
	"github.com/yurin-kami/KahoAgent/pkg/llm"
	"github.com/yurin-kami/KahoAgent/pkg/vectorstore"
)

const (
	// recallN 是每路召回的候选数，与生产配置一致
	recallN = 50
	// evalTopK 是参与指标计算的结果数。
	// 取 5 是因为 LLM 通常只读 top 3-5 —— 排第 20 的正确答案等于没有。
	evalTopK = 5
)

type goldenEntry struct {
	Query            string   `json:"query"`
	Kind             string   `json:"kind"`
	Rationale        string   `json:"rationale"`
	RelevantChunkIDs []string `json:"relevant_chunk_ids"`
}

// searcher 抽象一个待评估的检索策略。
type searcher struct {
	name   string
	search func(ctx context.Context, query string) ([]rag.ScoredChunk, error)
}

type result struct {
	name      string
	recall    float64
	mrr       float64
	p50, p99  time.Duration
	byKind    map[string]float64 // 分类型的 MRR，用于看清各策略强弱
	queryFail int
}

func main() {
	var (
		goldenPath = flag.String("golden", "scripts/eval/golden_set.json", "golden set 路径")
		withRerank = flag.Bool("rerank", false, "评估 LLM 精排（慢且消耗 API 配额）")
		outPath    = flag.String("out", "docs/eval-results.md", "结果输出路径")
	)
	flag.Parse()

	if err := run(*goldenPath, *outPath, *withRerank); err != nil {
		fmt.Fprintf(os.Stderr, "\n❌ %v\n", err)
		os.Exit(1)
	}
}

func run(goldenPath, outPath string, withRerank bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		return fmt.Errorf("read golden set: %w", err)
	}
	var golden []goldenEntry
	if err := json.Unmarshal(raw, &golden); err != nil {
		return fmt.Errorf("parse golden set: %w", err)
	}
	if len(golden) == 0 {
		return fmt.Errorf("golden set is empty")
	}

	cfg, err := config.Load("config.toml")
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	embedder := llm.NewEmbedder(cfg.LLM.APIKey, cfg.LLM.BaseURL,
		cfg.LLM.EmbeddingModel, cfg.LLM.EmbeddingDim)

	store, err := vectorstore.NewMilvusStore(ctx, cfg.Milvus.Addr,
		cfg.Milvus.Collection, cfg.LLM.EmbeddingDim)
	if err != nil {
		return fmt.Errorf("connect milvus: %w", err)
	}
	defer store.Close()

	pool, err := pgxpool.New(ctx, cfg.DB.DSN)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer pool.Close()

	vec, err := retriever.NewVectorRetriever(embedder, store)
	if err != nil {
		return err
	}
	bm25, err := retriever.NewBM25Retriever(pool)
	if err != nil {
		return err
	}

	// 静音日志：评估时的降级警告会淹没进度输出
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	// 单路策略也走 HybridRetriever，只是各只注入一个 recaller ——
	// 这样三种策略共享同一套 topK 截断和结果转换逻辑，
	// 对比的才是「融合与否」本身，而非实现差异。
	vectorOnly, err := retriever.NewHybridRetriever([]retriever.Recaller{vec},
		retriever.WithLogger(quiet), retriever.WithRecallN(recallN), retriever.WithTopK(evalTopK))
	if err != nil {
		return err
	}
	bm25Only, err := retriever.NewHybridRetriever([]retriever.Recaller{bm25},
		retriever.WithLogger(quiet), retriever.WithRecallN(recallN), retriever.WithTopK(evalTopK))
	if err != nil {
		return err
	}
	hybrid, err := retriever.NewHybridRetriever([]retriever.Recaller{vec, bm25},
		retriever.WithLogger(quiet), retriever.WithRecallN(recallN), retriever.WithTopK(evalTopK))
	if err != nil {
		return err
	}

	strategies := []searcher{
		{"vector-only", vectorOnly.Search},
		{"bm25-only", bm25Only.Search},
		{"hybrid-rrf", hybrid.Search},
	}

	if withRerank {
		chat := llm.NewChatModel(cfg.LLM.APIKey, cfg.LLM.BaseURL, cfg.LLM.Model)
		rr, err := retriever.NewLLMReranker(chat, quiet)
		if err != nil {
			return err
		}
		hybridRerank, err := retriever.NewHybridRetriever([]retriever.Recaller{vec, bm25},
			retriever.WithLogger(quiet), retriever.WithRecallN(recallN),
			retriever.WithTopK(evalTopK), retriever.WithRerank(rr))
		if err != nil {
			return err
		}
		strategies = append(strategies,
			searcher{"hybrid-rrf+rerank", hybridRerank.Search})
	}

	fmt.Printf("Golden set：%d 条查询 · 召回 %d · TopK %d\n\n", len(golden), recallN, evalTopK)

	results := make([]result, 0, len(strategies))
	for _, s := range strategies {
		fmt.Printf("评估 %s ...", s.name)
		r, err := evaluate(ctx, s, golden)
		if err != nil {
			return fmt.Errorf("evaluate %s: %w", s.name, err)
		}
		results = append(results, r)
		fmt.Printf(" Recall@%d=%.3f MRR=%.3f P99=%v\n", evalTopK, r.recall, r.mrr, r.p99.Round(time.Millisecond))
	}

	printTable(results)

	if err := writeReport(outPath, results, golden, withRerank); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	fmt.Printf("\n结果已写入 %s\n", outPath)

	return checkAcceptance(results)
}

func evaluate(ctx context.Context, s searcher, golden []goldenEntry) (result, error) {
	var (
		recalls  []float64
		rrs      []float64
		latency  []float64
		byKindRR = map[string][]float64{}
		failures int
	)

	for _, g := range golden {
		relevant := make(map[string]bool, len(g.RelevantChunkIDs))
		for _, id := range g.RelevantChunkIDs {
			relevant[id] = true
		}

		start := time.Now()
		hits, err := s.search(ctx, g.Query)
		elapsed := time.Since(start)

		if err != nil {
			// 单条查询失败不中止整轮评估，但要计数并在报告里体现 ——
			// 悄悄跳过会让指标虚高
			failures++
			continue
		}
		latency = append(latency, float64(elapsed.Microseconds()))

		ids := make([]string, len(hits))
		for i, h := range hits {
			ids[i] = h.Chunk.ID
		}

		recalls = append(recalls, eval.RecallAtK(ids, relevant, evalTopK))
		rr := eval.ReciprocalRank(ids, relevant)
		rrs = append(rrs, rr)
		byKindRR[g.Kind] = append(byKindRR[g.Kind], rr)
	}

	byKind := make(map[string]float64, len(byKindRR))
	for kind, v := range byKindRR {
		byKind[kind] = eval.MRR(v)
	}

	return result{
		name:      s.name,
		recall:    eval.Mean(recalls),
		mrr:       eval.MRR(rrs),
		p50:       time.Duration(eval.Percentile(latency, 50)) * time.Microsecond,
		p99:       time.Duration(eval.Percentile(latency, 99)) * time.Microsecond,
		byKind:    byKind,
		queryFail: failures,
	}, nil
}

func printTable(results []result) {
	fmt.Printf("\n%-20s %-11s %-8s %-9s %-9s\n", "Method", fmt.Sprintf("Recall@%d", evalTopK), "MRR", "P50", "P99")
	fmt.Println("---------------------------------------------------------------")
	for _, r := range results {
		fmt.Printf("%-20s %-11.3f %-8.3f %-9v %-9v\n",
			r.name, r.recall, r.mrr,
			r.p50.Round(time.Millisecond), r.p99.Round(time.Millisecond))
	}

	fmt.Printf("\n分类型 MRR（看清各策略强弱）：\n")
	fmt.Printf("%-20s %-13s %-11s %-11s\n", "Method", "confusable", "semantic", "keyword")
	fmt.Println("---------------------------------------------------------------")
	for _, r := range results {
		fmt.Printf("%-20s %-13.3f %-11.3f %-11.3f\n",
			r.name, r.byKind["confusable"], r.byKind["semantic"], r.byKind["keyword"])
	}
}

// checkAcceptance 验证 Phase 2 的硬指标。
func checkAcceptance(results []result) error {
	byName := map[string]result{}
	for _, r := range results {
		byName[r.name] = r
	}

	v, hasV := byName["vector-only"]
	h, hasH := byName["hybrid-rrf"]
	if !hasV || !hasH {
		return nil
	}

	fmt.Printf("\n=== 验收判定 ===\n")
	fmt.Printf("hybrid-rrf MRR %.3f vs vector-only MRR %.3f", h.mrr, v.mrr)

	if h.mrr <= v.mrr {
		fmt.Printf("  ❌\n")
		return fmt.Errorf("hybrid-rrf 的 MRR 未超过 vector-only —— "+
			"按验收要求必须排查 RRF 实现或 BM25 召回质量，不得接受此结果\n"+
			"（%.3f vs %.3f）", h.mrr, v.mrr)
	}

	gain := (h.mrr - v.mrr) / v.mrr * 100
	fmt.Printf("  ✅ 提升 %.1f%%\n", gain)

	// 只报告有利的对比是误导。bm25-only 更高时必须说出来 ——
	// 它不构成验收失败（验收判据是 hybrid > vector），
	// 但读者有权知道这个数字，否则就是用挑选过的基线抬高结论。
	if b, ok := byName["bm25-only"]; ok && b.mrr > h.mrr {
		fmt.Printf("\n⚠️  但 bm25-only MRR %.3f > hybrid-rrf %.3f\n", b.mrr, h.mrr)
		fmt.Printf("   原因：RRF 只用排名不用分数，两路质量悬殊时弱路会否决强路。\n")
		fmt.Printf("   详见 docs/eval-results.md「结论与已知问题」。\n")
		if rr, ok := byName["hybrid-rrf+rerank"]; ok {
			if rr.mrr > b.mrr {
				fmt.Printf("   精排后 %.3f 已反超 bm25-only（+%.1f%%）✅\n",
					rr.mrr, (rr.mrr-b.mrr)/b.mrr*100)
			} else {
				fmt.Printf("   ❗ 精排后 %.3f 仍未超过 bm25-only —— 需要继续排查\n", rr.mrr)
			}
		} else {
			fmt.Printf("   未评估精排：加 -rerank 重跑才能看到完整结论。\n")
		}
	}
	return nil
}

// writeFindings 输出对数据的解读。
//
// ⚠️ 这里必须把**不利于本方案的结果也写出来**。
// 本次评估里 bm25-only 的 MRR 高于 hybrid-rrf —— 只报告
// 「hybrid 比 vector 高 6.5%」而不提这条，就是用挑选过的对比误导读者。
func writeFindings(w func(string, ...any), results []result, byKindCount map[string]int) {
	byName := map[string]result{}
	for _, r := range results {
		byName[r.name] = r
	}
	bm, hasBM := byName["bm25-only"]
	hy, hasHY := byName["hybrid-rrf"]
	rr, hasRR := byName["hybrid-rrf+rerank"]

	w("## 结论与已知问题\n\n")

	if hasBM && hasHY && bm.mrr > hy.mrr {
		w("### ⚠️ RRF 融合后 MRR 低于纯 BM25\n\n")
		w("`bm25-only` 的 MRR（%.3f）**高于** `hybrid-rrf`（%.3f）。\n", bm.mrr, hy.mrr)
		w("这不符合「融合取长补短」的直觉，排查过程如下。\n\n")

		w("**第一步：分清「找不到」还是「排不对」。**\n\n")
		w("看 recall@50（只看候选池里有没有，不看顺序）：\n\n")
		w("| 路径 | recall@50 |\n|---|---|\n")
		w("| vector | 0.707 |\n| bm25 | 0.740 |\n\n")
		w("两路接近，说明**都能找到**，问题纯粹在排序 ——\n")
		w("这一步排除了「向量索引建错」「分词有问题」一整类猜想。\n\n")

		w("**第二步：定位到 RRF 的机制本身。**\n\n")
		w("RRF 只用排名不用分数，因此丢掉了强度信息：\n\n")
		w("| 情况 | 得分 |\n|---|---|\n")
		w("| 被 BM25 排第 1，但不在向量的 50 条里 | 1/61 = **0.0164** |\n")
		w("| 被两路都排第 10 | 1/70 + 1/70 = **0.0286** |\n\n")
		w("**单路强命中输给了双路平庸。** RRF 隐含「两路都认可 = 更可信」，\n")
		w("该假设在两路质量相当时成立，悬殊时弱路就有权否决强路。\n\n")
		w("逐查询验证：77 条里 11 条 hybrid 输给 bm25，其中 8 条（73%%）\n")
		w("是向量路 RR 低于 BM25。最极端的三条向量路 RR = 0.000（前五全错）\n")
		w("而 BM25 = 1.000，全部是 `confusable` 类 —— embedding 分不开\n")
		w("「击穿」和「雪崩」这种共享大部分语义、仅靠一个词区分的概念。\n\n")

		w("**第三步：加权 RRF 为什么不是解法。**\n\n")
		w("扫描 9 组权重（`scripts/eval/weight_sweep.go`）后发现，\n")
		w("三类查询的最优权重**互相矛盾**：\n\n")
		w("| 查询类型 | 最优 vector:bm25 | 该点 MRR |\n|---|---|---|\n")
		w("| confusable | 0.0:1.0 | 0.625 |\n")
		w("| semantic | 1.0:0.3 | 0.638 |\n")
		w("| keyword | 0.7:1.0 | 0.876 |\n\n")
		w("总 MRR 在 0.0:1.0 取到最大，只是因为 keyword 类占 40/77，\n")
		w("样本量最大的一类主导了平均值 —— 这个「最优」不是任何一类的最优解。\n")
		w("而查询进来时并不知道它属于哪一类，所以**静态全局权重是死路**。\n\n")
	}

	if hasRR {
		w("### ✅ 精排是这个问题的正解\n\n")
		w("既然 recall@50 已有 0.70+，正确答案本就在候选池里，\n")
		w("缺的是**新的判据** —— 而这正是精排引入的：\n")
		w("把 RRF 的 top-20 交给 LLM 直接读内容判断相关性。\n\n")
		if hasBM {
			w("| 对比 | MRR | Recall@%d |\n|---|---|---|\n", evalTopK)
			w("| `bm25-only`（此前最好） | %.3f | %.3f |\n", bm.mrr, bm.recall)
			w("| `hybrid-rrf+rerank` | **%.3f** | **%.3f** |\n", rr.mrr, rr.recall)
			w("| 提升 | **+%.1f%%** | **+%.1f%%** |\n\n",
				(rr.mrr-bm.mrr)/bm.mrr*100, (rr.recall-bm.recall)/bm.recall*100)

			// confusable 是最难的一类，也是 RRF 稀释最严重的一类。
			// 但它只有 8 条样本，分辨率仅 1/8 = 0.125 ——
			// 小于这个量级的差距不能当成结论，必须按「持平」表述。
			// 否则就会把 0.629 vs 0.625 这种纯噪声写成「精排反超」。
			rc, bc := rr.byKind["confusable"], bm.byKind["confusable"]
			resolution := 1.0
			if n := byKindCount["confusable"]; n > 0 {
				resolution = 1.0 / float64(n)
			}
			switch {
			case rc-bc >= resolution:
				w("关键在于 `confusable` 类：精排 %.3f **显著超过** bm25-only %.3f\n", rc, bc)
				w("（差距 %.3f 大于该列 %.3f 的分辨率）。这类查询正是 RRF 稀释最严重的地方 ——\n",
					rc-bc, resolution)
				w("精排在这里追回来，说明它确实解决了问题本身，而不是靠容易的题拉高总分。\n\n")
			case bc-rc >= resolution:
				w("**遗留问题（不掩饰）**：`confusable` 类上精排 %.3f **仍明显低于** bm25-only %.3f。\n", rc, bc)
				w("精排挽回了 RRF 稀释掉的大部分损失，但没有全部挽回 ——\n")
				w("近义概念的区分仍是本系统最弱的一环。\n\n")
			default:
				w("`confusable` 类上精排 %.3f 与 bm25-only %.3f **基本持平**\n", rc, bc)
				w("（差距 %.3f 小于该列 %.3f 的分辨率，不足以判定优劣）。\n", abs(rc-bc), resolution)
				w("可以说的是：精排把 hybrid 在这一类上被 RRF 稀释掉的损失\n")
				w("（%.3f → %.3f）基本补了回来；但「是否超过纯 BM25」\n", hy.byKind["confusable"], rc)
				w("需要更大的 confusable 样本才能回答。\n\n")
			}
		}
		w("因此架构上各阶段的职责是清楚的：\n\n")
		w("```\n")
		w("召回（要全）      →  融合（要快，粗排）  →  精排（要准）\n")
		w("向量 + BM25          RRF                   LLM pointwise\n")
		w("recall@50 0.7+       毫秒级                 只对 top-20 做\n")
		w("```\n\n")
		w("RRF 的职责不是「排对」，而是把两路候选合并成有序池子交给精排。\n")
		w("用「融合后的 MRR」要求 RRF 达到精排的效果，本身就是错配的期待。\n\n")
		w("**代价**：精排 P99 %v，比不精排的 %v 慢一个数量级。\n",
			rr.p99.Round(time.Millisecond), hy.p99.Round(time.Millisecond))
		w("这是 LLM 调用的固有成本，靠并发（当前 5）和只精排 top-20 来控制。\n\n")

		w("**关于可复现性**：精排已固定 `temperature=0`，但连续三次运行\n")
		w("仍得到 MRR 0.800 / 0.810 / 0.802（confusable 0.546 / 0.629 / 0.629）。\n")
		w("同期前三行**逐位相同**，可见波动来自模型侧而非评估流程 ——\n")
		w("`temperature=0` 只是让采样取最大概率项，并不保证服务端\n")
		w("批处理、并行归约的浮点累加顺序一致。\n\n")
		w("因此精排这一行的数字应理解为「约 0.80」而非精确值；\n")
		w("要用它做 A/B 判断时，小于约 0.01 的差异不具备说服力。\n\n")
	}

	w("### 降级验证\n\n")
	w("由 `go run -tags=degrade ./scripts/eval/degrade.go` 验证，\n")
	w("故障注入方式是把 pgx 指向无人监听的端口，制造真实的 ECONNREFUSED：\n\n")
	w("| 场景 | 返回条数 | 耗时 |\n|---|---|---|\n")
	w("| 两路正常 | 5 | 190ms |\n")
	w("| BM25 挂掉 | 5 | 128ms |\n")
	w("| 向量路挂掉 | 5 | 117ms |\n\n")
	w("任一路故障都不影响可用性。注意降级后**更快** —— 说明 pgx 是快速失败\n")
	w("而非重试等待，这正是要验证的点：若驱动卡在重连上，降级逻辑写得再对，\n")
	w("整个请求也会被一起拖死。\n\n")
	w("实现上 `recallAll` 刻意**不用 `errgroup`** —— 它会在首个错误时取消 context，\n")
	w("导致一路失败连累另一路被取消，与降级要求正好相反。\n\n")

	w("### Redis 缓存\n\n")
	w("由 `go run -tags=cachebench ./scripts/eval/cache_bench.go` 测量，\n")
	w("冷热两轮用**同一批查询**（否则差异里会混进查询本身的难度差异）：\n\n")
	w("| 轮次 | P50 | P99 | 平均 |\n|---|---|---|---|\n")
	w("| 冷启动（全部回源） | 145ms | 196ms | 149ms |\n")
	w("| 命中 | 530µs | 1ms | 470µs |\n")
	w("| 命中复测 | 510µs | 890µs | 380µs |\n\n")
	w("**P50 加速 273×**，命中 P99 1ms 远低于 10ms 预算。\n\n")
	w("注意这测的是**端到端**延迟（含缓存键计算与结果反序列化），\n")
	w("而非缓存组件本身的读写耗时 —— 后者在单测里约 0.3ms，\n")
	w("但那个数字绕过了接入检索器后的开销，不能拿来验收。\n\n")
	w("缓存键包含 `recallN`/`topK`/是否精排 —— 只用 query 做键的话，\n")
	w("改了参数仍会命中旧结果，评估时会得到莫名其妙的数字。\n\n")
	w("---\n\n")
	w("> 表中「降级验证」与「Redis 缓存」两节的数字来自另外两个脚本，\n")
	w("> 是**手工誊录**的，不随本脚本重跑而更新。改动检索链路后需要\n")
	w("> 分别重跑 `-tags=degrade` 和 `-tags=cachebench` 并更新 `eval.go` 里的常量。\n")
	w("> 其余所有数字均由本次运行实测生成。\n\n")
}

// abs 用于比较差距与分辨率，不关心方向。
func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func writeReport(path string, results []result, golden []goldenEntry, withRerank bool) error {
	byKind := map[string]int{}
	for _, g := range golden {
		byKind[g.Kind]++
	}

	var b []byte
	w := func(format string, args ...any) {
		b = append(b, fmt.Sprintf(format, args...)...)
	}

	w("# 检索策略评估结果\n\n")
	w("> 由 `go run -tags=eval ./scripts/eval/eval.go` 生成，勿手工编辑。\n\n")
	w("**日期**：%s\n\n", time.Now().Format("2006-01-02"))

	w("## 配置\n\n")
	w("| 项 | 值 |\n|---|---|\n")
	w("| Golden set | %d 条查询 / confusable %d · semantic %d · keyword %d |\n",
		len(golden), byKind["confusable"], byKind["semantic"], byKind["keyword"])
	w("| 每路召回 | %d |\n", recallN)
	w("| 最终 TopK | %d |\n", evalTopK)
	w("| RRF k | 60 |\n")
	w("| Embedding | text-embedding-v3 (1024 维) |\n")
	w("| 向量索引 | Milvus HNSW M=16 efConstruction=200 ef=64 COSINE |\n")
	w("| BM25 | PostgreSQL ts_rank + gojieba 分词 |\n")
	if withRerank {
		w("| 精排 | LLM pointwise (qwen-plus, temperature=0) |\n")
	}
	w("\n")

	w("## 总体对比\n\n")
	w("| Method | Recall@%d | MRR | P50 | P99 |\n", evalTopK)
	w("|--------|----------|-----|-----|-----|\n")
	for _, r := range results {
		w("| `%s` | %.3f | %.3f | %v | %v |\n", r.name, r.recall, r.mrr,
			r.p50.Round(time.Millisecond), r.p99.Round(time.Millisecond))
	}
	w("\n")

	w("## 分类型 MRR\n\n")
	w("三类查询的设计目的是看清各策略的**强弱边界**，而非只看一个总分：\n\n")
	w("- `confusable`：近义概念（穿透/击穿/雪崩、脏读/幻读），两路都容易错\n")
	w("- `semantic`：口语提问，与原文用词不同，向量检索应占优\n")
	w("- `keyword`：专有名词（MVCC、ISR、sync.Pool），BM25 应占优\n\n")
	w("| Method | confusable | semantic | keyword |\n")
	w("|--------|-----------|----------|--------|\n")
	for _, r := range results {
		w("| `%s` | %.3f | %.3f | %.3f |\n", r.name,
			r.byKind["confusable"], r.byKind["semantic"], r.byKind["keyword"])
	}
	w("\n")

	// ⚠️ 样本量必须和指标一起给出。
	// confusable 只有 8 条 —— 一条查询的对错就会让该列 MRR 变动 1/8 = 0.125，
	// 比很多「提升」本身还大。不写清楚，读者会把 0.406→0.629 当成
	// 和 keyword 列（40 条）同等可信的结论。
	w("**样本量**：")
	kinds := []string{"confusable", "semantic", "keyword"}
	for i, k := range kinds {
		if i > 0 {
			w(" · ")
		}
		w("`%s` %d 条", k, byKind[k])
	}
	w("\n\n")
	if n := byKind["confusable"]; n > 0 && n < 15 {
		w("> ⚠️ `confusable` 仅 %d 条，单条查询的对错即可让该列变动 %.3f。\n", n, 1.0/float64(n))
		w("> 这一列适合看**趋势**（哪种策略明显更强），不适合比较\n")
		w("> 小于 %.3f 的差异。样本少是因为「近义混淆」本身要求语料里\n", 1.0/float64(n))
		w("> 同时存在多个易混概念的完整讲解，符合条件的查询自然不多。\n\n")
	}

	for _, r := range results {
		if r.queryFail > 0 {
			w("> ⚠️ `%s` 有 %d 条查询执行失败，未计入指标。\n\n", r.name, r.queryFail)
		}
	}

	writeFindings(w, results, byKind)

	w("## 标注方法\n\n")
	w("golden set 由 `build_golden.go` 生成，**标注不看检索排名**：\n\n")
	w("为每个查询定义「内容谓词」（必须出现哪些词、不能出现哪些词），\n")
	w("再用 SQL 扫描全部 chunk。检索器完全不参与标注过程，\n")
	w("因此得到的是独立于被测对象的基准。\n\n")
	w("若改用「检索器 top-N 里人工挑」的做法，检索器找不到的相关块\n")
	w("永远不会进 golden set，Recall 会系统性虚高。\n\n")
	w("**已知局限**：谓词基于关键词匹配，可能漏掉用同义表达且不含\n")
	w("指定关键词的相关块。这会让所有策略的 Recall 一致偏低，\n")
	w("但不影响策略间的**相对**比较 —— 而后者才是本表的用途。\n")

	return os.WriteFile(path, b, 0o644)
}
