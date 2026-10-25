-- 消息引用索引。
--
-- 背景：QQ 官方机器人把「引用一条消息」下发成 message_type=103 + msg_elements，
-- 元素里只有被引用正文，**不含发送者**（实测 0/11）。想显示「回复了谁的消息」
-- 只能靠本地反查：入站事件里每条消息自带 msg_idx（REFIDX_…），
-- 别人引用它时事件里的 ref_msg_idx 就等于这个值。
--
-- 所以这里把 msg_idx 单独存一列并建索引：
--   1. 拼提示词时要按 ref_msg_idx 找到被引消息的发送者
--   2. WebUI 的引用预览也要用同一条查询
--
-- 为什么不用 additional（JSON 列）反查：SQLite 无法对 JSON 值建可用的索引，
-- 每次查都要全表扫描 + 解析 JSON。聊天记录一多就是明显卡顿。
ALTER TABLE messages ADD COLUMN ref_msg_idx TEXT NOT NULL DEFAULT '';

-- 回填历史数据：把已经存在 additional 里的 msg_idx 提出来。
--
-- 只在 additional 是合法 JSON 且带 msg_idx 时才动，避免脏数据让迁移整体失败。
-- json_extract 对非法 JSON 会抛错，所以先用 json_valid 挡一层。
UPDATE messages
SET ref_msg_idx = COALESCE(json_extract(additional, '$.msg_idx'), '')
WHERE ref_msg_idx = ''
  AND json_valid(additional) = 1
  AND json_extract(additional, '$.msg_idx') IS NOT NULL;

-- 按引用索引查被引消息是热路径（每轮拼提示词、每屏 WebUI 预览）。
-- 部分索引：只索引非空值，绝大多数消息没有引用索引。
CREATE INDEX IF NOT EXISTS idx_messages_ref_msg_idx
    ON messages(session_id, ref_msg_idx)
    WHERE ref_msg_idx <> '';
