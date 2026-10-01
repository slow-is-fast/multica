-- Ruel: Run 产物落点（P0-6 要求的 diff 与测试证据）。
--
-- 上游把 Run 结果塞进 agent_task_queue.result 这个 jsonb，只有 output / pr_url /
-- work_dir / session_id 四项，没有 diff，也没有测试证据。本表补上这个落点。
--
-- 表名带 ruel_ 前缀是有意的：这是 fork 侧新增的表，与上游命名空间隔开。上游一旦
-- 自己加了 artifacts，不会撞名，也不会让 sqlc 生成物产生冲突。

CREATE TABLE IF NOT EXISTS ruel_artifacts (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid        NOT NULL,
    task_id      uuid        NOT NULL,
    issue_id     uuid,
    kind         text        NOT NULL,
    uri          text        NOT NULL DEFAULT '',
    size         bigint      NOT NULL DEFAULT 0,
    -- 硬约束：产物是验收的唯一证据，允许没有校验值的产物等于让「测试通过」这句
    -- 话失去可核对性。数据库层面强制非空，而不是靠调用方自觉。
    checksum     text        NOT NULL CHECK (length(checksum) > 0),
    content      text,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_ruel_artifacts_task
    ON ruel_artifacts (task_id);

CREATE INDEX IF NOT EXISTS idx_ruel_artifacts_issue
    ON ruel_artifacts (issue_id)
    WHERE issue_id IS NOT NULL;

-- 同一 Run 的同类产物只保留一条最新记录（按写入顺序覆盖）。
CREATE UNIQUE INDEX IF NOT EXISTS uq_ruel_artifacts_task_kind
    ON ruel_artifacts (task_id, kind);
