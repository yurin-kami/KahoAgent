-- Phase 2：BM25 全文检索所需的表结构。
--
-- 为什么 chunk 要在 PG 里再存一份（Milvus 已经有了）：
-- 两者索引的是**完全不同的东西** —— Milvus 存向量（语义），
-- PG 存词元（关键词）。混合检索的前提就是两条独立的召回路径。
--
-- 幂等：可重复执行。

CREATE TABLE IF NOT EXISTS chunks (
    -- 与 Milvus 中的主键完全一致：sha256(doc_id#index)[:32]
    -- 这是两个存储能对齐、RRF 能按 ID 融合的基础
    id           VARCHAR(64) PRIMARY KEY,
    doc_id       VARCHAR(64) NOT NULL,
    chunk_index  INTEGER     NOT NULL,
    content      TEXT        NOT NULL,

    -- tokens 是 gojieba 分好词后用空格连接的结果。
    -- ⚠️ 不能直接对 content 做 to_tsvector：PG 自带的分词器不认识中文，
    --    会把「Redis缓存穿透」当成一个不可分割的词，
    --    导致查「缓存穿透」完全匹配不上。
    --    故在 Go 侧用 gojieba 切好，PG 用 'simple' 分词器按空格切。
    tokens       TEXT        NOT NULL,

    metadata     JSONB       NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- 生成列：tokens 变更时自动重算，不会忘记同步
    content_tsv  TSVECTOR GENERATED ALWAYS AS (to_tsvector('simple', tokens)) STORED
);

-- GIN 索引是全文检索的性能关键。
-- 没有它，ts_rank 查询会退化成全表扫描 —— 几千行时感觉不出来，
-- 上万行后延迟会变得无法接受。
CREATE INDEX IF NOT EXISTS idx_chunks_tsv ON chunks USING GIN (content_tsv);

-- 支撑 DeleteByDoc 的按文档删除
CREATE INDEX IF NOT EXISTS idx_chunks_doc_id ON chunks (doc_id);
