-- 长期记忆、表情包、表达学习三块功能的 schema。
--
-- 设计取舍（重要，改之前先读）：
--
-- 1. 记忆的文本检索用 FTS5，不用向量。
--    原因：部署用的 OpenAI 兼容端点只提供 chat 模型，/v1/embeddings 全 404，
--    没有可用的 embedding 服务。参考实现（maisaka/memory/mid_term.py）
--    用向量算余弦相似度，这里退化成「关键词 + 分词倒排」，
--    检索质量差一档，但链路完整、不引入新依赖。
--    详见 internal/memory/tokenize.go 的说明。
--
-- 2. FTS 表用外部内容模式（content=''），自己维护 rowid。
--    不用 content=messages 是因为回想记录不在 messages 表里，
--    两个来源共用一张 FTS 表更省事，代价是删除时要手动同步。
--
-- 3. 时间列沿用既有的 INTEGER 秒，与 0001_init.sql 保持一致。
--    已知代价：秒级精度，同秒内排序要带 id 作次级键。

-- ============ 长期记忆：聊天回想 ============
-- 对应 mai_term.py 的 MidTermMemorySummaryModel，字段一一对应。
CREATE TABLE IF NOT EXISTS memory_summaries (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    -- session_id 为空表示全局记忆（跨会话共享，例如对某个人的长期印象）。
    session_id  TEXT    NOT NULL DEFAULT '',
    group_id    TEXT    NOT NULL DEFAULT '',
    -- summary 是完整摘要，保留话题脉络、人物立场、结论、待办。
    summary     TEXT    NOT NULL DEFAULT '',
    -- recall_cues 是 JSON 数组，用于之后语义匹配的查询式短句。
    recall_cues TEXT    NOT NULL DEFAULT '[]',
    -- participants 是 JSON 数组，参与人物显示名。
    participants TEXT   NOT NULL DEFAULT '[]',
    -- time_range 是摘要覆盖的时间范围，人类可读。
    time_range  TEXT    NOT NULL DEFAULT '',
    -- source_count 是被压缩了多少条原始消息，用于排查。
    source_count INTEGER NOT NULL DEFAULT 0,
    -- covered_until 是这些原始消息里最晚的一条时间戳（秒）。
    -- 增量摘要靠它判断「哪些消息还没被总结过」。
    covered_until INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_memory_summaries_session
    ON memory_summaries(session_id, created_at);

-- ============ 长期记忆：人物画像 ============
-- 对应 maisaka/memory/person_profile.py。
-- 每人每会话一份，profile 是结构化文本（Markdown 分节）。
CREATE TABLE IF NOT EXISTS person_profiles (
    platform    TEXT    NOT NULL DEFAULT 'qq',
    user_id     TEXT    NOT NULL,
    session_id  TEXT    NOT NULL DEFAULT '',
    display_name TEXT   NOT NULL DEFAULT '',
    -- profile_text 是画像正文，超过上限时在写入前截断。
    profile_text TEXT   NOT NULL DEFAULT '',
    -- message_count 是参与过多少条消息，用于判断要不要更新画像。
    message_count INTEGER NOT NULL DEFAULT 0,
    -- last_message_at 是最后一次触发更新的消息时间。
    last_message_at INTEGER NOT NULL DEFAULT 0,
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY(platform, user_id, session_id)
);

CREATE INDEX IF NOT EXISTS idx_person_profiles_user
    ON person_profiles(platform, user_id);

-- ============ 表情包 ============
-- 对应 emoji_system/emoji_manager.py 的 MaiEmoji。
CREATE TABLE IF NOT EXISTS emojis (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    -- file_hash 是内容的 sha256，天然去重。
    file_hash   TEXT    NOT NULL UNIQUE,
    -- file_path 是相对 data/ 的路径，便于整体搬迁。
    file_path   TEXT    NOT NULL DEFAULT '',
    -- description 是 VLM 生成的情绪标签，逗号分隔。
    description TEXT    NOT NULL DEFAULT '',
    -- 收藏来源，便于追溯。
    source_session TEXT NOT NULL DEFAULT '',
    source_user    TEXT NOT NULL DEFAULT '',
    -- usage_count 用于「高频表情优先」的加权抽样。
    usage_count INTEGER NOT NULL DEFAULT 0,
    last_used_at INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_emojis_usage ON emojis(usage_count DESC);

-- ============ 表达学习 ============
-- 对应 learners/expression_learner.py 的 Expression 数据模型。
-- situation + style 二元组来自 learn_style.prompt 的输出格式。
CREATE TABLE IF NOT EXISTS expressions (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    -- session_id 为空表示跨会话通用（群里学到的说法到处能用）。
    session_id  TEXT    NOT NULL DEFAULT '',
    situation   TEXT    NOT NULL DEFAULT '',
    style       TEXT    NOT NULL DEFAULT '',
    -- source_text 是学这条时的原始出处，便于人工核对。
    source_text TEXT    NOT NULL DEFAULT '',
    -- used_count / last_used_at 用于评优与淘汰。
    used_count  INTEGER NOT NULL DEFAULT 0,
    last_used_at INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_expressions_unique
    ON expressions(session_id, situation, style);

-- ============ 黑话 ============
-- 对应 learners/jargon_learner.py。
CREATE TABLE IF NOT EXISTS jargons (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    content     TEXT    NOT NULL,
    -- meaning 是推断出的含义，空表示还没学会。
    meaning     TEXT    NOT NULL DEFAULT '',
    -- context 是学习它时的上下文片段。
    context     TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_jargons_content ON jargons(content);

-- ============ 全文检索索引（外部内容模式） ============
--
-- tokens 列存的是自研分词结果（空格分隔），unicode61 按空格切词。
-- 为什么不用 trigram：实测 unicode61 与 trigram 都对中文短词失效，
-- trigram 查「火锅」在「去吃了火锅」里 0 命中，因为它只切三字组。
-- 自研分词把「去吃了火锅」切成「去 去吃 吃 吃了 了 了火 火 火锅 锅」，
-- 查「火锅」可精确命中。详见 internal/memory/tokenize.go。
CREATE VIRTUAL TABLE IF NOT EXISTS memory_fts USING fts5(
    content,
    tokens,
    tokenize = 'unicode61'
);

-- 表情包的文本索引：按情绪标签检索。
CREATE VIRTUAL TABLE IF NOT EXISTS emoji_fts USING fts5(
    description,
    tokens,
    tokenize = 'unicode61'
);
