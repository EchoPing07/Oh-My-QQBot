-- 黑话需要「命中次数」才能决定何时推断含义。
--
-- 为什么要单独一条迁移而不是改 0002：
-- 0002 已经在开发机上执行过（schema_migrations 里有记录），
-- 改它不会重跑，只会让已部署的库缺字段、新库有字段 —— 两种环境行为不一致。
-- 加一条新迁移是唯一安全的做法。
--
-- 字段与参考实现 jargon_miner.py 的 Jargon 模型对齐：
--   count                    命中次数，每次再次遇到就 +1
--   last_inference_count     上次推断含义时的 count，用来避免重复推断
ALTER TABLE jargons ADD COLUMN count INTEGER NOT NULL DEFAULT 1;

ALTER TABLE jargons ADD COLUMN last_inference_count INTEGER NOT NULL DEFAULT 0;
