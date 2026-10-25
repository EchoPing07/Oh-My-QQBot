-- 推理记录补「事件种类」与「工具执行结果」字段。
--
-- 为什么必须补：参考实现的监控时间线是**一本事件账本**，里面同时有
-- 消息、Planner 请求（planner.finalized）、工具执行（tool.execution）、
-- Replyer 响应（replier.response）、门控结果（timing_gate.result）、
-- 上下文裁切（flow.step: context.trimmed）等 8 类事件。
-- 我们原来只有 kind='planner' 一种，其它都是空白 ——
-- 界面上看到的只有「Planner 想了什么」，看不到「工具实际做了什么」。
--
-- 新增的列：
--   kind        已有，现在扩展取值：planner / tool / replyer / gate / trim
--   name        事件名（工具名、replyer、gate 动作等）
--   ok          tool/replyer 是否成功
--   duration_ms 该事件耗时（毫秒）
--   payload     该事件的补充信息（JSON，工具参数/结果、门控输入等）
--   parent      关联的上级事件（工具执行关联到它所属的 Planner round）
--
-- 为什么不新建表：时间线要按时间混排，同表排序最简单；
-- 且这些字段大多可空，新建表会多一次 JOIN。

ALTER TABLE reasoning_records ADD COLUMN name TEXT NOT NULL DEFAULT '';
ALTER TABLE reasoning_records ADD COLUMN ok INTEGER NOT NULL DEFAULT 1;
ALTER TABLE reasoning_records ADD COLUMN duration_ms INTEGER NOT NULL DEFAULT 0;
ALTER TABLE reasoning_records ADD COLUMN payload TEXT NOT NULL DEFAULT '';
ALTER TABLE reasoning_records ADD COLUMN parent INTEGER NOT NULL DEFAULT 0;

-- 时间线要按「会话 + 时间」取，kind 用于前端分流渲染。
CREATE INDEX IF NOT EXISTS idx_reasoning_session_ms_kind
  ON reasoning_records(session_id, created_at_ms, kind);
