-- Ruel: 项目知识条目的落点（PRD 第 5 章 5.3 节的第三类保留）。
--
-- PRD 要求「Run 结束产出结构化总结 → 高价值结论由人批准后进入项目知识 →
-- 后续 Run 能读到它」。总结本身是缓存（#50），这一张表装的是**经人审阅**的
-- 那一部分，也是唯一允许进入后续 Run 上下文的东西。
--
-- 刻意不叫 approved_knowledge_retention 或 approved_knowledge：PRD 5.3 给本类
-- 起的名字正是 approved_knowledge_retention（经审阅的项目知识），而上游
-- gcpolicy.go 里已有一个 approved_knowledge 类——它扫的是
-- hermes-state/<agent>/<profile>/memories/，即 **Hermes 在执行机上的文件型
-- 长期记忆**，是 Provider 原生记忆，不是本表这一类。两者同名不同物：合并显示
-- 之后没人分得清删的是哪个，所以表名带 ruel_ 前缀隔开命名空间。
--
-- 这一版**不**记录「总结的某个字段不可用」。不可用是**总结字段**的属性
-- （#46 结论 §3.2），而本表只在确实产出了决定时才有一行：不可用 ⇒ 没有行，
-- 不是「一行内容为空」。把两者混在一条记录里会丢掉「不可用」与「为空」的区别，
-- 那个区别由 #50 在总结记录上表达。
--
-- 表名带 ruel_ 前缀的另一条理由同 ruel_artifacts：fork 侧新增，与上游命名空间
-- 隔开，上游将来自己加 knowledge 表不会撞名，也不会让 sqlc 生成物冲突。

CREATE TABLE IF NOT EXISTS ruel_project_knowledge (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- 硬边界，不是归属字段。PRD 6.7 里程碑 3 的退出条件要求跨 workspace 不得
    -- 混入，NOT NULL 是这个约束在库层的表达：允许 NULL 就等于允许出现一条
    -- 「哪个项目都能读到」的知识，那正好是 PRD 第 5 章明令避免的全局记忆。
    --
    -- 外键不是装饰：只写 NOT NULL 的话，一个不存在的 workspace_id 照样能插进去，
    -- 「硬边界」就只剩文档里的一句话。CASCADE 与 comment_workspace_id_fkey 一致。
    workspace_id      uuid        NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,

    -- 条目正文：那条被批准的结论。空白串不算内容——一条空知识进了后续 Run 的
    -- 上下文，除了占 token 没有任何作用，而且它看起来是「有一条知识」。
    statement         text        NOT NULL CHECK (length(btrim(statement)) > 0),
    -- 依据：为什么这么决定。来自总结的 decisions[].why，可以为空（有些决定
    -- 就是没有可写的理由），但空串与「没采集到」在展示层必须分开。
    rationale         text        NOT NULL DEFAULT '',

    -- 来源：回溯到具体哪一次 Run / 哪条 Issue / 哪条评论。三列都可空，因为
    -- 条目也可能由人直接手写（编辑后批准），那时没有上游 Run。
    --
    -- 刻意**不加外键**，与 comment.source_task_id 的处理一致：溯源是引用性质，
    -- 必须比被引用的东西活得久。加了 ON DELETE SET NULL，删掉一次 Run 就会把
    -- 「这条知识从哪来」无声抹掉——而知识存在的意义正是在 Run 消失之后还在。
    -- 反过来不加 FOREIGN KEY，删除 Run 不会动这里，代价只是可能指向一个已不存在的行。
    --
    -- 可空但不许用零 UUID 占位：零 UUID 是一个看起来像外键的值，会让「没有来源」
    -- 与「来源是某个不存在的 Run」长得一样。
    source_task_id    uuid,
    source_issue_id   uuid,
    source_comment_id uuid,

    -- 审阅状态。#47 指定的四态。
    status            text        NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'approved', 'rejected', 'archived')),

    -- 审阅意见：拒绝或归档时的理由。这里是**人的意见**，与「总结字段不可用」
    -- 无关（见文件头）。默认空串而不是 NULL，与 rationale 同理。
    review_note       text        NOT NULL DEFAULT '',

    -- 审阅人。刻意不叫 approved_by：一个 status='rejected' 的行的
    -- approved_by 是个谎言。上游对同一问题的解法是 comment.resolved_by_type /
    -- resolved_by_id——按**动作**命名而不是按**结果**命名，这里照办。
    --
    -- reviewed_by_type 取值与 comment.author_type 对齐（member / agent /
    -- system / plugin）。允许 system 是有意的：将来若做自动归档，它必须以
    -- 显式 system 身份落地，而不是把 reviewed_by 留空绕过下面那条一致性约束。
    reviewed_by_type  text,
    reviewed_by_id    uuid,
    reviewed_at       timestamptz,

    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),

    -- 一致性：pending 必须没有任何审阅痕迹；其余三态必须齐全。
    -- 形状照 comment_resolved_consistency：把「状态说已经审过」与「审阅人是空的」
    -- 这种自相矛盾挡在库层，而不是靠调用方自觉。若不强约束，库里会出现一批
    -- status='approved' 而没人批准过的行——审批链路的整个意义就没了。
    CONSTRAINT ruel_project_knowledge_review_consistency CHECK (
        (status = 'pending'
             AND reviewed_at IS NULL AND reviewed_by_type IS NULL AND reviewed_by_id IS NULL)
        OR (status <> 'pending'
             AND reviewed_at IS NOT NULL AND reviewed_by_type IS NOT NULL AND reviewed_by_id IS NOT NULL)
    )
);

-- 注入用：后续 Run 读的就是「本 workspace 已批准的那几条」。
-- 部分索引精确对上那个查询，且只索引真正会被读到的行。
CREATE INDEX IF NOT EXISTS idx_ruel_project_knowledge_approved
    ON ruel_project_knowledge (workspace_id, created_at)
    WHERE status = 'approved';

-- 审批队列用（#48）：待审条目按 workspace 列。
CREATE INDEX IF NOT EXISTS idx_ruel_project_knowledge_pending
    ON ruel_project_knowledge (workspace_id, created_at)
    WHERE status = 'pending';

-- 回溯用：从一次 Run 反查它产出了哪些条目。
CREATE INDEX IF NOT EXISTS idx_ruel_project_knowledge_source_task
    ON ruel_project_knowledge (source_task_id)
    WHERE source_task_id IS NOT NULL;
