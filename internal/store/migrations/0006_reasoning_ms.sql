-- 推理记录补毫秒精度。
--
-- 为什么必须补：WebUI 要把「思考」和「消息」按时间插到同一条时间线里
-- （参考实现是主事件账本 timeline，消息与推理混排）。
-- 但 reasoning_records.created_at 只有秒级，而 messages.created_at_ms 是毫秒，
-- 同一秒内先发消息再思考的情况下无法判断谁在前 —— 表现是
-- 思考块偶尔跑到它对应的那条消息上面去。
--
-- 另外同一轮里的多个 Planner round 会落在同一秒（实测 turn 内
-- round 1 与 round 2 相隔 3 秒，但更快的模型会到亚秒级），
-- 只按秒排序会得到不稳定的顺序。

ALTER TABLE reasoning_records ADD COLUMN created_at_ms INTEGER NOT NULL DEFAULT 0;

-- 历史数据：秒 ×1000。虽然仍无亚秒精度，但至少与 created_at 口径一致，
-- 不会出现「新记录排在旧记录前面」。
UPDATE reasoning_records SET created_at_ms = created_at * 1000 WHERE created_at_ms = 0;

-- 时间线查询按会话 + 毫秒取。
CREATE INDEX IF NOT EXISTS idx_reasoning_session_ms
  ON reasoning_records(session_id, created_at_ms);
