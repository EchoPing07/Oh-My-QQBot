-- 记忆向量化：从「关键词检索」改回「语义检索」。
--
-- 背景（改之前先读）：
--   第一版因为误判「端点没有 embedding 服务」（当时用一个不存在的模型名试探，
--   拿到 not found 就当成端点不支持），把检索降级成了 FTS5 + 自研分词。
--   实测证据表明这个降级代价很大 —— 同一批数据下：
--     查询「我们之前说过要一起去吃好吃的」→ FTS5 命中 0 条，向量检索命中正确答案
--     查询「模型响应太慢怎么办」        → FTS5 命中 0 条，向量检索命中正确答案
--     查询「他有什么爱好」              → FTS5 命中 0 条，向量检索命中正确答案
--   原因是关键词检索要求字面重合，而用户/模型的问法几乎从不与原文重合。
--   现在恢复向量检索，FTS5 保留作为降级路径。
--
-- 存储格式：float32 小端字节串（1024 维 = 4KB）。
-- 不用 JSON：JSON 约 12KB，每次检索要读回几十条，差值直接体现在延迟上。
ALTER TABLE memory_summaries ADD COLUMN embedding BLOB;

-- 记录向量化时用的模型与维度。
-- 换模型后维度可能变，旧向量必须能识别出来并跳过，否则余弦相似度算出来是垃圾。
ALTER TABLE memory_summaries ADD COLUMN embed_model TEXT NOT NULL DEFAULT '';

ALTER TABLE memory_summaries ADD COLUMN embed_dim INTEGER NOT NULL DEFAULT 0;

-- 索引：只有向量为空的记录才需要回填，这个部分索引让回填查询不必全表扫。
CREATE INDEX IF NOT EXISTS idx_memory_summaries_pending_embed
    ON memory_summaries(id) WHERE embedding IS NULL;
