//go:build spike

// Spike S3：验证 Milvus 读写连通性。
//
// 目的：
//  1. 证明 Milvus 服务可达
//  2. 亲手走一遍 建表 → 插入 → Flush → 建索引 → Load → 检索 的完整流程
//  3. 踩一遍两个必踩的坑（不 Flush 搜不到 / 不 Load 报错），
//     Phase 1 写 vectorstore 时就不会再被这两个坑卡住
//
// 运行：go run -tags=spike scripts/spike/milvus/main.go
// 成功标志：查询向量 {1,0,0,0} 返回 id=a 排第一、id=c 第二。
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/milvus-io/milvus-sdk-go/v2/client"
	"github.com/milvus-io/milvus-sdk-go/v2/entity"

	"github.com/yurin-kami/KahoAgent/internal/config"
)

const (
	collName = "spike_test"
	dim      = 4 // spike 用小维度便于手算；真实项目见 config.embedding_dim
)

func main() {
	cfg, err := config.Load("config.toml")
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// ── 1. 连接 ───────────────────────────────────────────
	fmt.Printf("connecting to %s ...\n", cfg.Milvus.Addr)
	cli, err := client.NewClient(ctx, client.Config{Address: cfg.Milvus.Addr})
	if err != nil {
		log.Fatalf("connect failed: %v\n(检查 podman-compose ps，milvus 是否 healthy)", err)
	}
	defer cli.Close()
	fmt.Println("✅ 连接成功")

	// 清理上次残留，保证可重复运行
	if has, _ := cli.HasCollection(ctx, collName); has {
		_ = cli.DropCollection(ctx, collName)
	}

	// ── 2. 建 Collection ──────────────────────────────────
	// PrimaryKey 必须显式标记；VarChar 必须指定 MaxLength。
	schema := entity.NewSchema().
		WithName(collName).
		WithField(entity.NewField().
			WithName("id").
			WithDataType(entity.FieldTypeVarChar).
			WithIsPrimaryKey(true).
			WithMaxLength(64)).
		WithField(entity.NewField().
			WithName("vector").
			WithDataType(entity.FieldTypeFloatVector).
			WithDim(dim))

	if err := cli.CreateCollection(ctx, schema, 1); err != nil {
		log.Fatalf("create collection: %v", err)
	}
	fmt.Println("✅ Collection 创建成功")

	// ── 3. 插入 ───────────────────────────────────────────
	// Milvus 是列式插入 —— 不是一行一行插，是一列一列插。
	ids := []string{"a", "b", "c"}
	vecs := [][]float32{
		{1, 0, 0, 0},
		{0, 1, 0, 0},
		{0.9, 0.1, 0, 0}, // 与 a 最接近
	}
	if _, err := cli.Insert(ctx, collName, "",
		entity.NewColumnVarChar("id", ids),
		entity.NewColumnFloatVector("vector", dim, vecs),
	); err != nil {
		log.Fatalf("insert: %v", err)
	}

	// ⚠️ 坑 1：不 Flush 数据还在内存 buffer，搜不到
	if err := cli.Flush(ctx, collName, false); err != nil {
		log.Fatalf("flush: %v", err)
	}
	fmt.Println("✅ 插入 3 条并 Flush")

	// ── 4. 建索引 ─────────────────────────────────────────
	// COSINE：文本向量用余弦相似度；M=16 / efConstruction=200 是社区推荐值。
	idx, err := entity.NewIndexHNSW(entity.COSINE, 16, 200)
	if err != nil {
		log.Fatalf("build index config: %v", err)
	}
	if err := cli.CreateIndex(ctx, collName, "vector", idx, false); err != nil {
		log.Fatalf("create index: %v", err)
	}

	// ⚠️ 坑 2：搜索前必须 Load，把索引加载进内存
	if err := cli.LoadCollection(ctx, collName, false); err != nil {
		log.Fatalf("load collection: %v", err)
	}
	fmt.Println("✅ HNSW 索引创建并加载")

	// ── 5. 检索 ───────────────────────────────────────────
	sp, err := entity.NewIndexHNSWSearchParam(64) // ef 必须 ≥ topK
	if err != nil {
		log.Fatalf("build search param: %v", err)
	}

	start := time.Now()
	results, err := cli.Search(ctx, collName,
		nil,                     // partitions
		"",                      // expr：标量过滤，Phase 2 会用
		[]string{"id"},          // outputFields
		[]entity.Vector{entity.FloatVector([]float32{1, 0, 0, 0})},
		"vector",                // vectorField
		entity.COSINE,           // 必须与建索引时一致
		2,                       // topK
		sp,
	)
	if err != nil {
		log.Fatalf("search: %v", err)
	}
	elapsed := time.Since(start)

	fmt.Printf("\n✅ 检索成功 (耗时 %v)\n", elapsed.Round(time.Microsecond))
	for _, r := range results {
		idCol, ok := r.Fields.GetColumn("id").(*entity.ColumnVarChar)
		if !ok {
			log.Fatalf("unexpected column type for id")
		}
		fmt.Printf("命中 %d 条:\n", r.ResultCount)
		for i := 0; i < r.ResultCount; i++ {
			v, _ := idCol.ValueByIdx(i)
			fmt.Printf("  %d. id=%s  score=%.4f\n", i+1, v, r.Scores[i])
		}
	}
	fmt.Println("\n预期: 1. id=a (1.0000)   2. id=c (~0.9939)")

	// 清理
	_ = cli.DropCollection(ctx, collName)
}
