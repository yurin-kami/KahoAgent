//go:build buildgolden

// build_golden 从内容判定生成 golden set。
//
//	go run -tags=buildgolden ./scripts/eval/build_golden.go
//
// ⚠️ 方法论：标注**不看检索排名**。
//
// 若拿检索器的 top-N 当候选再挑，标注结果就被检索器的偏好污染了 ——
// 它找不到的相关块永远不会进 golden set，于是 Recall 天然虚高。
// 这里改用「内容谓词」：为每个查询定义「一个块要算相关必须满足什么」，
// 然后用 SQL 扫**全量** 1653 个块。检索器完全不参与，
// 因此得到的是独立于被测对象的基准。
//
// 谓词写法遵循两条原则：
//  1. 宁严勿松 —— 标错比标少更伤评估可信度。多标一个不相关的块，
//     会让所有策略的分数一起虚高，掩盖真实差距。
//  2. 必须能人工复核 —— 每条谓词都对应一句自然语言的判定标准，
//     生成后抽样核对。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yurin-kami/KahoAgent/internal/config"
)

// criterion 定义一个查询的相关性判定标准。
type criterion struct {
	Query string
	Kind  string
	// Rationale 是这条判定的自然语言说明，供人工复核
	Rationale string
	// AllOf：这些词必须全部出现（用于锁定具体概念）
	AllOf []string
	// AnyOf：至少命中一个（用于同义表达）
	AnyOf []string
	// NoneOf：出现任一即排除（用于排除近义干扰项）
	NoneOf []string
	// MinLen 过滤掉过短的块 —— 一两句话的块即便含关键词也
	// 很难说「回答了问题」
	MinLen int
}

const defaultMinLen = 80

// criteria 是全部查询的判定标准。
//
// 三类查询的设计目的（对应 L2.1）：
//   - confusable：近义概念，两路检索都容易错，最能体现 RRF+ReRank 价值
//   - semantic：口语提问与原文用词不同，向量检索应占优
//   - keyword：专有名词，BM25 应占优
var criteria = []criterion{
	// ===== C 类：近义混淆（最重要） =====
	{
		Query: "怎么防止缓存被恶意请求打穿", Kind: "confusable",
		Rationale: "必须讲缓存穿透的解法（布隆过滤器/缓存空值），排除只讲击穿或雪崩的块",
		AllOf:     []string{"穿透"},
		AnyOf:     []string{"布隆", "空值", "不存在的"},
	},
	{
		Query: "热点key突然失效导致数据库压力暴增怎么办", Kind: "confusable",
		Rationale: "必须讲缓存击穿的解法（互斥锁/逻辑过期）",
		AllOf:     []string{"击穿"},
		AnyOf:     []string{"互斥锁", "逻辑过期", "热点", "SETNX"},
	},
	{
		Query: "大量缓存同时过期怎么处理", Kind: "confusable",
		Rationale: "必须讲缓存雪崩的解法（随机TTL/多级缓存）",
		AllOf:     []string{"雪崩"},
		AnyOf:     []string{"随机", "TTL", "多级", "熔断", "同时过期"},
	},
	{
		Query: "MySQL 里读到别的事务没提交的数据是什么问题", Kind: "confusable",
		Rationale: "必须提到脏读概念",
		AllOf:     []string{"脏读"},
	},
	{
		Query: "同一个查询两次结果不一样是什么隔离级别问题", Kind: "confusable",
		Rationale: "必须提到不可重复读",
		AllOf:     []string{"不可重复读"},
	},
	{
		Query: "事务中两次范围查询返回的行数变了", Kind: "confusable",
		Rationale: "讲幻读及其解决（间隙锁），排除只顺带提一句幻读的块",
		AllOf:     []string{"幻读"},
		AnyOf:     []string{"间隙锁", "next-key", "Next-Key", "范围"},
	},
	{
		Query: "Go 里协程和线程有什么区别", Kind: "confusable",
		Rationale: "讲 goroutine 与线程的具体差异（栈大小/调度开销）",
		AllOf:     []string{"线程"},
		AnyOf:     []string{"2KB", "用户态", "轻量级"},
	},
	{
		Query: "进程和线程的区别", Kind: "confusable",
		Rationale: "讲操作系统层面的进程与线程对比，排除只讲 goroutine 的块",
		AllOf:     []string{"进程"},
		AnyOf:     []string{"线程", "内核态"},
	},

	// ===== A 类：语义 =====
	{
		Query: "数据库查询太慢了怎么优化", Kind: "semantic",
		Rationale: "讲慢查询定位方法",
		AnyOf:     []string{"慢查询日志", "EXPLAIN"},
	},
	{
		Query: "怎么保证消息不丢", Kind: "semantic",
		Rationale: "讲消息可靠投递的具体机制",
		AllOf:     []string{"acks"},
	},
	{
		Query: "服务挂了怎么自动重连", Kind: "semantic",
		Rationale: "讲重连与退避策略",
		AllOf:     []string{"重连"},
		AnyOf:     []string{"退避", "指数", "backoff"},
	},
	{
		Query: "多台机器怎么保证同一时间只有一个在执行", Kind: "semantic",
		Rationale: "讲分布式锁的具体实现",
		AllOf:     []string{"分布式锁"},
		AnyOf:     []string{"SETNX", "Redlock", "续期", "看门狗"},
	},
	{
		Query: "怎么生成全局唯一的ID", Kind: "semantic",
		Rationale: "讲雪花算法的位分配结构",
		AnyOf:     []string{"雪花", "snowflake", "Snowflake"},
		AllOf:     []string{"位"},
	},
	{
		Query: "用户登录状态怎么保存", Kind: "semantic",
		Rationale: "讲 JWT 或 Session 认证机制",
		AnyOf:     []string{"JWT", "Session", "会话"},
		AllOf:     []string{"认证"},
	},
	{
		Query: "程序内存一直涨是什么原因", Kind: "semantic",
		Rationale: "讲内存泄漏或 GC 行为",
		AnyOf:     []string{"内存泄漏", "垃圾回收", "三色标记"},
	},
	{
		Query: "怎么让接口响应更快", Kind: "semantic",
		Rationale: "讲具体的性能优化手段而非泛泛提到缓存",
		AnyOf:     []string{"性能优化", "响应时间", "QPS"},
		AllOf:     []string{"优化"},
		MinLen:    200,
	},
	{
		Query: "两个服务之间怎么通信比较好", Kind: "semantic",
		Rationale: "讲通信协议选型权衡",
		AllOf:     []string{"协议"},
		AnyOf:     []string{"选型", "权衡", "取舍"},
	},
	{
		Query: "大文件上传怎么做", Kind: "semantic",
		Rationale: "讲分片上传",
		AnyOf:     []string{"分片上传", "multipart", "分块上传"},
	},
	{
		Query: "密码应该怎么存到数据库", Kind: "semantic",
		Rationale: "讲 bcrypt 等密码哈希方案",
		AllOf:     []string{"bcrypt"},
	},
	{
		Query: "怎么知道文件有没有重复上传过", Kind: "semantic",
		Rationale: "讲秒传或文件指纹去重",
		AnyOf:     []string{"秒传", "文件哈希", "文件指纹"},
	},
	{
		Query: "并发写同一条记录会有什么问题", Kind: "semantic",
		Rationale: "讲死锁的成因与处理",
		AllOf:     []string{"死锁"},
		AnyOf:     []string{"回滚", "等待图", "检测"},
	},
	{
		Query: "怎么做限流", Kind: "semantic",
		Rationale: "讲限流算法",
		AnyOf:     []string{"令牌桶", "漏桶", "限流"},
		AllOf:     []string{"限流"},
	},
	{
		Query: "前端点了按钮怎么立刻有反馈", Kind: "semantic",
		Rationale: "讲乐观 UI",
		AnyOf:     []string{"乐观 UI", "乐观UI", "optimistic"},
	},
	{
		Query: "网络断了消息怎么补发", Kind: "semantic",
		Rationale: "讲离线消息补发或重传机制",
		AnyOf:     []string{"离线消息", "补发", "重传"},
	},

	// ===== B 类：关键词 =====
	{Query: "MVCC", Kind: "keyword", Rationale: "讲 MVCC 实现机制", AllOf: []string{"MVCC"}, AnyOf: []string{"Read View", "ReadView", "undo", "版本链"}},
	{Query: "GMP 调度模型", Kind: "keyword", Rationale: "讲 GMP 三者关系与队列", AllOf: []string{"GMP"}, AnyOf: []string{"本地队列", "全局队列", "窃取", "stealing"}},
	{Query: "B+树索引", Kind: "keyword", Rationale: "讲 B+ 树叶子节点结构", AnyOf: []string{"B+树", "B+ 树"}, AllOf: []string{"叶子"}},
	{Query: "sync.Pool", Kind: "keyword", Rationale: "讲 sync.Pool", AllOf: []string{"sync.Pool"}},
	{Query: "context.WithCancel", Kind: "keyword", Rationale: "讲 context 取消传播", AnyOf: []string{"WithCancel", "WithTimeout"}},
	{Query: "Redis AOF RDB", Kind: "keyword", Rationale: "讲 AOF 重写或混合持久化", AllOf: []string{"AOF"}, AnyOf: []string{"重写", "混合持久化", "appendfsync"}},
	{Query: "Redis Cluster 槽位", Kind: "keyword", Rationale: "讲集群分片槽位", AnyOf: []string{"16384", "哈希槽", "槽位"}},
	{Query: "ZSet 跳表", Kind: "keyword", Rationale: "讲跳表结构", AnyOf: []string{"跳表", "skiplist"}},
	{Query: "Kafka ISR", Kind: "keyword", Rationale: "讲 ISR 副本同步", AllOf: []string{"ISR"}},
	{Query: "Kafka 零拷贝 sendfile", Kind: "keyword", Rationale: "讲零拷贝原理", AnyOf: []string{"零拷贝", "sendfile"}},
	{Query: "TCP 三次握手", Kind: "keyword", Rationale: "讲三次握手过程", AnyOf: []string{"三次握手", "四次挥手"}},
	{Query: "HTTP/2 多路复用", Kind: "keyword", Rationale: "讲 HTTP/2 的多路复用与头部压缩", AllOf: []string{"HTTP/2"}, AnyOf: []string{"多路复用", "HPACK", "头部压缩"}},
	{Query: "bcrypt cost", Kind: "keyword", Rationale: "讲 bcrypt 参数", AllOf: []string{"bcrypt"}},
	{Query: "S3 multipart upload", Kind: "keyword", Rationale: "讲 S3 分片上传", AnyOf: []string{"multipart", "分片上传"}, AllOf: []string{"S3"}},
	{Query: "gRPC 双向流", Kind: "keyword", Rationale: "讲 gRPC 双向流式通信", AllOf: []string{"gRPC"}, AnyOf: []string{"双向流", "bidirectional"}},
	{Query: "select case channel", Kind: "keyword", Rationale: "讲 select 的阻塞与随机选择行为", AllOf: []string{"select"}, AnyOf: []string{"default", "随机", "阻塞"}},
	{Query: "defer recover panic", Kind: "keyword", Rationale: "讲 Go panic 恢复", AllOf: []string{"recover"}},
	{Query: "slice 扩容", Kind: "keyword", Rationale: "讲 slice 底层扩容策略", AllOf: []string{"slice"}, AnyOf: []string{"扩容", "底层数组"}},
	{Query: "map 并发安全", Kind: "keyword", Rationale: "讲 map 并发问题", AnyOf: []string{"sync.Map", "并发读写"}},
	{Query: "两阶段提交 2PC", Kind: "keyword", Rationale: "讲 2PC 协议", AnyOf: []string{"2PC", "两阶段提交"}},
	{Query: "TCC 补偿事务", Kind: "keyword", Rationale: "讲 TCC 模式", AllOf: []string{"TCC"}},
	{Query: "雪花算法时钟回拨", Kind: "keyword", Rationale: "讲时钟回拨处理", AnyOf: []string{"时钟回拨"}},
	{Query: "Redlock", Kind: "keyword", Rationale: "讲 Redlock 算法", AnyOf: []string{"Redlock", "RedLock"}},
	{Query: "间隙锁 next-key lock", Kind: "keyword", Rationale: "讲间隙锁如何防幻读", AnyOf: []string{"间隙锁", "next-key", "Next-Key"}, AllOf: []string{"锁"}},
	{Query: "WezTerm tab 关闭", Kind: "keyword", Rationale: "讲 WezTerm 具体问题", AnyOf: []string{"WezTerm", "wezterm"}},
	{Query: "ImGui 破坏性变更", Kind: "keyword", Rationale: "讲 ImGui 版本升级问题", AnyOf: []string{"ImGui", "imgui"}, AllOf: []string{"OptiScaler"}},
	{Query: "UTF-8 编译错误", Kind: "keyword", Rationale: "讲 UTF-8 导致的具体编译报错", AnyOf: []string{"BOM", "C2001", "C4819", "execution-charset"}},
	{Query: "CJK 字体渲染", Kind: "keyword", Rationale: "讲 CJK 字形范围加载", AnyOf: []string{"CJK"}},
	{Query: "pgx 连接池", Kind: "keyword", Rationale: "讲 pgx 用法", AnyOf: []string{"pgx", "pgxpool"}},
	{Query: "微服务拆分粒度", Kind: "keyword", Rationale: "讲微服务拆分原则", AnyOf: []string{"服务拆分", "拆分粒度"}},
	{Query: "设计模式 Go 实现", Kind: "keyword", Rationale: "讲具体设计模式", AnyOf: []string{"单例模式", "工厂模式", "策略模式", "观察者模式"}},

	// ===== 补充查询：达到规划要求的 50+ 规模 =====
	{Query: "Redis 为什么这么快", Kind: "semantic", Rationale: "讲 Redis 高性能的具体原因", AllOf: []string{"Redis"}, AnyOf: []string{"单线程", "IO 多路复用", "epoll"}},
	{Query: "Kafka 为什么吞吐量高", Kind: "semantic", Rationale: "讲 Kafka 高吞吐设计", AllOf: []string{"Kafka"}, AnyOf: []string{"顺序写", "页缓存", "批量发送"}},
	{Query: "索引失效的情况有哪些", Kind: "semantic", Rationale: "讲索引失效场景", AnyOf: []string{"索引失效", "最左前缀"}},
	{Query: "回表是什么意思", Kind: "keyword", Rationale: "讲回表概念", AnyOf: []string{"回表"}},
	{Query: "覆盖索引", Kind: "keyword", Rationale: "讲覆盖索引优化", AnyOf: []string{"覆盖索引"}},
	{Query: "分库分表怎么做", Kind: "semantic", Rationale: "讲分库分表策略", AnyOf: []string{"分库分表", "水平拆分"}},
	{Query: "主从复制原理", Kind: "semantic", Rationale: "讲主从同步机制", AllOf: []string{"从库"}, AnyOf: []string{"binlog", "主从复制"}},
	{Query: "Redis 过期键怎么删除", Kind: "semantic", Rationale: "讲过期删除策略", AnyOf: []string{"惰性删除", "定期删除"}},
	{Query: "Redis 内存满了怎么办", Kind: "semantic", Rationale: "讲内存淘汰策略", AnyOf: []string{"淘汰策略", "maxmemory", "allkeys-lru"}},
	{Query: "goroutine 泄漏", Kind: "keyword", Rationale: "讲 goroutine 泄漏成因", AnyOf: []string{"goroutine 泄漏", "协程泄漏"}},
	{Query: "GC 三色标记", Kind: "keyword", Rationale: "讲三色标记与写屏障", AnyOf: []string{"三色标记", "写屏障"}},
	{Query: "channel 关闭后再读会怎样", Kind: "semantic", Rationale: "讲 channel 关闭语义", AllOf: []string{"close"}, AnyOf: []string{"零值", "已关闭"}},
	{Query: "WaitGroup 用法", Kind: "keyword", Rationale: "讲 WaitGroup", AnyOf: []string{"WaitGroup"}},
	{Query: "Mutex 和 RWMutex 区别", Kind: "confusable", Rationale: "讲读写锁与互斥锁差异", AllOf: []string{"RWMutex"}},
	{Query: "幂等性怎么保证", Kind: "semantic", Rationale: "讲幂等设计", AnyOf: []string{"幂等"}},
	{Query: "Kafka 消费者组 rebalance", Kind: "keyword", Rationale: "讲 rebalance 机制", AnyOf: []string{"rebalance", "重平衡", "再均衡"}},
	{Query: "消息重复消费怎么处理", Kind: "semantic", Rationale: "讲重复消费与去重", AnyOf: []string{"重复消费"}},
	{Query: "Saga 模式", Kind: "keyword", Rationale: "讲 Saga 分布式事务", AnyOf: []string{"Saga", "SAGA"}},
	{Query: "本地消息表", Kind: "keyword", Rationale: "讲本地消息表方案", AnyOf: []string{"本地消息表"}},
	{Query: "CAP 定理", Kind: "keyword", Rationale: "讲 CAP 三选二", AnyOf: []string{"CAP"}},
	{Query: "缓存和数据库怎么保证一致", Kind: "semantic", Rationale: "讲双写一致性方案", AnyOf: []string{"延迟双删", "双写一致"}},
	{Query: "布隆过滤器误判率", Kind: "keyword", Rationale: "讲布隆过滤器原理与误判", AllOf: []string{"布隆"}, AnyOf: []string{"误判", "哈希函数", "位数组"}},
	{Query: "MySQL 慢查询怎么排查", Kind: "semantic", Rationale: "讲慢查询排查流程", AnyOf: []string{"慢查询日志", "EXPLAIN"}},
	{Query: "深分页问题", Kind: "semantic", Rationale: "讲深分页优化", AnyOf: []string{"深分页", "limit 100000", "分页优化"}},
	{Query: "TCP 粘包怎么解决", Kind: "semantic", Rationale: "讲粘包拆包", AnyOf: []string{"粘包", "拆包"}},
}

type goldenEntry struct {
	Query            string   `json:"query"`
	Kind             string   `json:"kind"`
	Rationale        string   `json:"rationale"`
	RelevantChunkIDs []string `json:"relevant_chunk_ids"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cfg, err := config.Load("config.toml")
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	pool, err := pgxpool.New(ctx, cfg.DB.DSN)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer pool.Close()

	var total int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM chunks").Scan(&total); err != nil {
		return fmt.Errorf("count chunks: %w", err)
	}
	fmt.Printf("语料共 %d 个 chunk，逐条应用内容谓词（不看检索排名）\n\n", total)

	entries := make([]goldenEntry, 0, len(criteria))
	var (
		skipped  []string
		labelSum int
	)

	for _, c := range criteria {
		ids, err := match(ctx, pool, c)
		if err != nil {
			return fmt.Errorf("match %q: %w", c.Query, err)
		}

		// 少于 1 条说明谓词写得不对或语料里确实没有 —— 两种情况都不该进
		// golden set：前者会让评估失真，后者会让所有策略都得 0 分，
		// 无法区分优劣。
		if len(ids) == 0 {
			skipped = append(skipped, c.Query+"（无匹配）")
			continue
		}
		// 匹配过多说明谓词太松，标注不可信。
		//
		// 上限定 20 而非更小：语料是主题集中的技术笔记，一篇讲 B+ 树的
		// 文章本就会切出十几个都与「B+树索引」相关的块 —— 那是语料的性质，
		// 不是谓词的问题。真正的风险是谓词命中了**不相关**内容，
		// 这由谓词本身的严格度控制（AllOf 锁定核心概念），
		// 而非靠压低数量上限。
		//
		// 超过 20 条则几乎可以肯定谓词过泛（如只用「缓存」「线程」这类
		// 高频词），此时标注不可信，宁可弃用该查询。
		if len(ids) > 20 {
			skipped = append(skipped, fmt.Sprintf("%s（匹配 %d 条，谓词过松）", c.Query, len(ids)))
			continue
		}

		entries = append(entries, goldenEntry{
			Query:            c.Query,
			Kind:             c.Kind,
			Rationale:        c.Rationale,
			RelevantChunkIDs: ids,
		})
		labelSum += len(ids)
		fmt.Printf("[%-10s] %-40s %2d 条相关\n", c.Kind, truncate(c.Query, 40), len(ids))
	}

	if len(skipped) > 0 {
		fmt.Printf("\n⚠️  跳过 %d 条：\n", len(skipped))
		for _, s := range skipped {
			fmt.Printf("   - %s\n", s)
		}
	}

	out, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if err := os.WriteFile("scripts/eval/golden_set.json", out, 0o644); err != nil {
		return fmt.Errorf("write golden set: %w", err)
	}

	fmt.Printf("\n✅ golden set 已生成：%d 条查询 / %d 条标注 / 平均 %.1f 条相关\n",
		len(entries), labelSum, float64(labelSum)/float64(len(entries)))

	byKind := map[string]int{}
	for _, e := range entries {
		byKind[e.Kind]++
	}
	fmt.Printf("   分布：confusable %d / semantic %d / keyword %d\n",
		byKind["confusable"], byKind["semantic"], byKind["keyword"])
	return nil
}

// match 在全量 chunk 上应用谓词，返回命中的 chunk ID。
func match(ctx context.Context, pool *pgxpool.Pool, c criterion) ([]string, error) {
	minLen := c.MinLen
	if minLen == 0 {
		minLen = defaultMinLen
	}

	// 用参数化查询构造条件。全表扫描在 1653 行规模下毫无压力，
	// 且这是离线脚本，不必优化。
	var (
		conds []string
		args  []any
	)
	add := func(cond string, val any) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf(cond, len(args)))
	}

	add("length(content) >= $%d", minLen)
	for _, kw := range c.AllOf {
		add("content ILIKE '%%' || $%d || '%%'", kw)
	}
	if len(c.AnyOf) > 0 {
		var ors []string
		for _, kw := range c.AnyOf {
			args = append(args, kw)
			ors = append(ors, fmt.Sprintf("content ILIKE '%%' || $%d || '%%'", len(args)))
		}
		conds = append(conds, "("+strings.Join(ors, " OR ")+")")
	}
	for _, kw := range c.NoneOf {
		add("content NOT ILIKE '%%' || $%d || '%%'", kw)
	}

	q := "SELECT id FROM chunks WHERE " + strings.Join(conds, " AND ") + " ORDER BY id"

	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
