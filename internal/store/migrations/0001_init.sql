-- 初始 schema。
-- 表设计与 MaiBot 的 database_model 对应，只保留精简版需要的部分。

-- 消息表：全量消息落库，用于上下文重建、统计与回放。
CREATE TABLE IF NOT EXISTS messages (
    id          TEXT PRIMARY KEY,
    session_id  TEXT    NOT NULL,
    message_id  TEXT    NOT NULL,
    platform    TEXT    NOT NULL DEFAULT 'qq',
    user_id     TEXT    NOT NULL,
    user_name   TEXT    NOT NULL DEFAULT '',
    group_id    TEXT    NOT NULL DEFAULT '',
    group_name  TEXT    NOT NULL DEFAULT '',
    is_group    INTEGER NOT NULL DEFAULT 0,
    is_at       INTEGER NOT NULL DEFAULT 0,
    is_mentioned INTEGER NOT NULL DEFAULT 0,
    is_command  INTEGER NOT NULL DEFAULT 0,
    segments    TEXT    NOT NULL DEFAULT '[]',
    plain_text  TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_messages_session_time ON messages(session_id, created_at);
CREATE INDEX IF NOT EXISTS idx_messages_message_id ON messages(message_id);
CREATE INDEX IF NOT EXISTS idx_messages_created_at ON messages(created_at);

-- 聊天流表。
CREATE TABLE IF NOT EXISTS sessions (
    id             TEXT PRIMARY KEY,
    platform       TEXT    NOT NULL DEFAULT 'qq',
    account_id     TEXT    NOT NULL DEFAULT '',
    scope          TEXT    NOT NULL,
    target_id      TEXT    NOT NULL,
    display_name   TEXT    NOT NULL DEFAULT '',
    message_count  INTEGER NOT NULL DEFAULT 0,
    last_active_at INTEGER NOT NULL,
    created_at     INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sessions_last_active ON sessions(last_active_at);

-- 用户表。与聊天流无关，跨群共享。
CREATE TABLE IF NOT EXISTS persons (
    platform    TEXT    NOT NULL DEFAULT 'qq',
    user_id     TEXT    NOT NULL,
    nickname    TEXT    NOT NULL DEFAULT '',
    card_name   TEXT    NOT NULL DEFAULT '',
    first_seen  INTEGER NOT NULL,
    last_seen   INTEGER NOT NULL,
    PRIMARY KEY(platform, user_id)
);

-- 模型调用记录，用于 token 与花费统计。
CREATE TABLE IF NOT EXISTS llm_usage (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    task              TEXT    NOT NULL,
    model             TEXT    NOT NULL,
    session_id        TEXT    NOT NULL DEFAULT '',
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens      INTEGER NOT NULL DEFAULT 0,
    cost              REAL    NOT NULL DEFAULT 0,
    duration_ms       INTEGER NOT NULL DEFAULT 0,
    created_at        INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_llm_usage_created ON llm_usage(created_at);
CREATE INDEX IF NOT EXISTS idx_llm_usage_task ON llm_usage(task, created_at);

-- 键值表，用于 schema 版本、OpenID 映射、运行时状态等。
CREATE TABLE IF NOT EXISTS kv (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL
);

-- 推理记录，用于 WebUI 的推理重放。
CREATE TABLE IF NOT EXISTS reasoning_records (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT    NOT NULL,
    turn_id    TEXT    NOT NULL DEFAULT '',
    round      INTEGER NOT NULL DEFAULT 0,
    kind       TEXT    NOT NULL DEFAULT 'planner',
    request    TEXT    NOT NULL DEFAULT '',
    response   TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_reasoning_session ON reasoning_records(session_id, created_at);
