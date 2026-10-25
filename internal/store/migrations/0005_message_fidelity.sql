-- 消息表保真度修复。
--
-- 三个问题一起解决，都是「知道有坑但一直没修」的：
--
-- 1. created_at 只有秒级精度
--    ReceivedAt 的纳秒部分落库即丢，读回来恒为 :00。
--    引用公式里 `previous_seconds <= 3` 这类分档在重启后会偏，
--    因为间隔精度掉到 1 秒。新增 created_at_ms 存毫秒，
--    排序与恢复都用它；created_at 保留给按时间过滤的旧查询。
--
-- 2. 群名片 / 账号 / 平台附加字段不落库
--    UserCardName、AccountID、Additional（OpenID 映射）原来完全丢弃，
--    重启恢复的上下文里这些字段是空的，group_card 属性直接消失。
--
-- 3. 机器人自己的发言从不入库
--    出站消息只进内存 botReplies（上限 20 条、重启即丢），
--    于是 WebUI 会话页只有用户消息，重启后上下文只有半边。
--    新增 is_self 标记出站消息。

ALTER TABLE messages ADD COLUMN created_at_ms   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE messages ADD COLUMN user_card_name  TEXT    NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN account_id      TEXT    NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN additional      TEXT    NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN is_self         INTEGER NOT NULL DEFAULT 0;

-- 历史数据的毫秒值等于秒值 ×1000，虽然仍无亚秒精度，
-- 但至少和 created_at 口径一致，不会出现「新消息排在旧消息前面」。
UPDATE messages SET created_at_ms = created_at * 1000 WHERE created_at_ms = 0;

-- 恢复上下文按毫秒倒序取，这条索引是那条查询的必需品。
CREATE INDEX IF NOT EXISTS idx_messages_session_ms ON messages(session_id, created_at_ms);
