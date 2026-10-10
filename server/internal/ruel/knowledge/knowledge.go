// Package knowledge 提供项目知识条目的落点（PRD 第 5 章 5.3 节的第三类保留）。
//
// 它与 ruel/artifacts 是同一层的两个邻居，形状刻意保持一致：手写查询、不依赖
// sqlc 生成物。理由同 artifacts 的包注释——重新生成 sqlc 会大面积改动
// pkg/db/generated，那正是将来同步上游时冲突最集中的地方。
//
// 本包刻意**不叫** approved_knowledge：上游 gcpolicy 里已有一个 approved_knowledge
// 类，它扫的是执行机上的 Hermes 文件型长期记忆（hermes-state/<agent>/<profile>/
// memories/），是 provider 原生记忆。两者同名不同物，合并之后没人分得清删的是哪个。
//
// 本包也不记录「总结的某个字段不可用」。不可用是**总结字段**的属性：不可用 ⇒
// 没有条目，而不是「一条内容为空的条目」。把两者混起来会丢掉「不可用」与「为空」
// 的区别，那个区别由 #50 在总结记录上表达。
package knowledge

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// 审阅状态。四态的取值与迁移 565 的 CHECK 逐字对应——两处必须一起改，所以
// 这里不做「宽容解析」，未知值一律拒绝。
const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusRejected = "rejected"
	StatusArchived = "archived"
)

// 审阅人类型，取值与 comment.author_type 对齐。
//
// 允许 system 是有意的：将来若做自动归档，它必须以显式的 system 身份落地。
// 不给这个取值，实现者要么把 reviewed_by 留空（违反库层的一致性约束），要么
// 伪造一个 member——两条路都比多一个常量差。
const (
	ReviewerMember = "member"
	ReviewerAgent  = "agent"
	ReviewerSystem = "system"
	ReviewerPlugin = "plugin"
)

// ErrNotPending 表示对一条已经审过的条目又提交了审批。
//
// 单独定义而不是复用某个泛化错误：它是并发/重复点击的**正常**结果，调用方要把它
// 映射成 409 而不是 500。用挂起的 status 构造，让错误信息直接说清当前是什么态。
type ErrNotPending struct{ Status string }

func (e ErrNotPending) Error() string {
	return fmt.Sprintf("knowledge: 条目当前是 %q，只有待审条目能被批准或拒绝", e.Status)
}

// ErrUnknownStatus 表示状态或审阅人类型不在已知取值里。
//
// 收紧而不放行任意字符串：状态是界面上那句话的索引，放进来一个没见过的词，
// 前端只能显示空白，缺陷就又被藏回去了。
var ErrUnknownStatus = errors.New("knowledge: 未知的状态取值")

// ErrNotFound 表示这条记录在本 workspace 下不存在。
//
// 「不存在」与「属于另一个 workspace」刻意用同一个错误：两者对外都只能说「这里
// 没有这条」，区分开就变成了一个探测别的项目有哪些条目的接口。
//
// 单独定义成哨兵而不是让调用方去比对错误文本：HTTP 层要按它回 404，按文本匹配
// 的话，改一句提示语就会让 404 静默退化成 500。
var ErrNotFound = errors.New("knowledge: 本 workspace 下没有这条记录")

// ValidStatus 判断一个字符串是不是已知的审阅状态。
func ValidStatus(status string) bool {
	switch strings.TrimSpace(status) {
	case StatusPending, StatusApproved, StatusRejected, StatusArchived:
		return true
	}
	return false
}

// ValidReviewerType 判断一个字符串是不是已知的审阅人类型。
func ValidReviewerType(t string) bool {
	switch strings.TrimSpace(t) {
	case ReviewerMember, ReviewerAgent, ReviewerSystem, ReviewerPlugin:
		return true
	}
	return false
}

// DBTX 是 pgx 的最小子集，形状与 handler 侧的 dbExecutor 一致。
type DBTX interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Entry 是一条项目知识条目。
//
// Source* 三列是**溯源**，可以指向一个已经不存在的 Run/Issue/评论（库里刻意没有
// 对应外键，见迁移 565 的注释）：知识存在的意义正是在 Run 消失之后还在。
type Entry struct {
	ID          pgtype.UUID
	WorkspaceID pgtype.UUID
	Statement   string
	Rationale   string

	SourceTaskID    pgtype.UUID
	SourceIssueID   pgtype.UUID
	SourceCommentID pgtype.UUID

	Status string
	// ReviewNote 是人的审阅意见，与「总结字段不可用」无关。
	//
	// ReviewNote 是 NOT NULL DEFAULT ''，可以直接用 string；ReviewedByType /
	// ReviewedByID / ReviewedAt 三列在待审时是 NULL，必须用 pgtype 承载——
	// 用 plain string 接 NULL 会让 pgx 在扫描时报错，而「待审」恰恰是最常见的状态。
	ReviewNote     string
	ReviewedByType pgtype.Text
	ReviewedByID   pgtype.UUID
	ReviewedAt     pgtype.Timestamptz

	CreatedAt pgtype.Timestamptz
	UpdatedAt pgtype.Timestamptz
}

const entryColumns = `id, workspace_id, statement, rationale,
       source_task_id, source_issue_id, source_comment_id,
       status, review_note, reviewed_by_type, reviewed_by_id, reviewed_at,
       created_at, updated_at`

// Store 是 ruel_project_knowledge 表的读写入口。
//
// 每一个读方法都**必须**接收 workspaceID 并把它写进 WHERE。这不是风格问题：
// workspace 是硬边界（PRD 6.7 里程碑 3 的退出条件），一个「查全部已批准条目」的
// 方法只要存在，早晚会有人在注入路径上调用它，于是知识跨项目泄漏。所以本包不提供
// 这样的方法，而不是靠调用方记得加过滤。
type Store struct{ db DBTX }

func NewStore(db DBTX) *Store { return &Store{db: db} }

// Create 新建一条待审条目。
//
// 状态不由调用方决定，恒为 pending：能直接创建一条 approved 的记录等于绕开审批，
// 而审批正是本表存在的理由。要让条目变为已批准，走 Review。
func (s *Store) Create(ctx context.Context, e Entry) (Entry, error) {
	if !e.WorkspaceID.Valid {
		return Entry{}, errors.New("knowledge: workspace_id 必填——它是硬边界，不是可选的归属字段")
	}
	statement := strings.TrimSpace(e.Statement)
	if statement == "" {
		return Entry{}, errors.New("knowledge: statement 不能为空")
	}
	row := s.db.QueryRow(ctx, `
INSERT INTO ruel_project_knowledge
    (workspace_id, statement, rationale, source_task_id, source_issue_id, source_comment_id, status)
VALUES ($1, $2, $3, $4, $5, $6, 'pending')
RETURNING `+entryColumns,
		e.WorkspaceID, statement, e.Rationale,
		e.SourceTaskID, e.SourceIssueID, e.SourceCommentID)
	return scanEntry(row)
}

// ListForWorkspace 列出某 workspace 的条目。status 传空串表示不筛。
func (s *Store) ListForWorkspace(ctx context.Context, workspaceID pgtype.UUID, status string) ([]Entry, error) {
	if !workspaceID.Valid {
		return nil, errors.New("knowledge: workspace_id 必填")
	}
	status = strings.TrimSpace(status)
	if status != "" && !ValidStatus(status) {
		return nil, fmt.Errorf("%w: %q", ErrUnknownStatus, status)
	}
	// 用 SQL 侧的可空参数而不是拼字符串：拼串在 status 为空的这一支上会把
	// 「筛 pending」和「不筛」写成两个不同的查询，日后改动容易只改一支。
	rows, err := s.db.Query(ctx, `
SELECT `+entryColumns+`
  FROM ruel_project_knowledge
 WHERE workspace_id = $1
   AND ($2 = '' OR status = $2)
 ORDER BY created_at DESC, id DESC`, workspaceID, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEntries(rows)
}

// ListApprovedForWorkspace 是注入路径要的那一个查询：本 workspace 已批准的全部条目。
//
// 只返回 approved，且只按 workspace 过滤——两个条件合起来就是「跨 workspace 不混入」
// 在读取侧的落地。
func (s *Store) ListApprovedForWorkspace(ctx context.Context, workspaceID pgtype.UUID) ([]Entry, error) {
	return s.ListForWorkspace(ctx, workspaceID, StatusApproved)
}

// Review 对一条**待审**条目落审批决定。decision 只接受 approved / rejected。
//
// 只允许从 pending 出发，且条件写在 UPDATE 的 WHERE 里而不是先查后写：先查后写在
// 两个审批人同时点击时会双写，后者覆盖前者而两边都看到成功。这里 UPDATE 影响 0 行
// 就说明别人已经处理过了，再回读一次当前状态构造 ErrNotPending——调用方拿到的
// 是一个能解释「为什么没生效」的错误，而不是一句泛化的失败。
func (s *Store) Review(ctx context.Context, id, workspaceID pgtype.UUID, decision, note, reviewerType string, reviewerID pgtype.UUID) (Entry, error) {
	decision = strings.TrimSpace(decision)
	if decision != StatusApproved && decision != StatusRejected {
		return Entry{}, fmt.Errorf("%w: 审批只接受 %s / %s，收到 %q",
			ErrUnknownStatus, StatusApproved, StatusRejected, decision)
	}
	if !ValidReviewerType(reviewerType) {
		return Entry{}, fmt.Errorf("%w: 审阅人类型 %q", ErrUnknownStatus, reviewerType)
	}
	if !reviewerID.Valid {
		return Entry{}, errors.New("knowledge: 批准人是必填的——一条没有批准人的已批准记录无法追责")
	}

	row := s.db.QueryRow(ctx, `
UPDATE ruel_project_knowledge
   SET status           = $3,
       review_note      = $4,
       reviewed_by_type = $5,
       reviewed_by_id   = $6,
       reviewed_at      = now(),
       updated_at       = now()
 WHERE id = $1 AND workspace_id = $2 AND status = 'pending'
RETURNING `+entryColumns,
		id, workspaceID, decision, note, reviewerType, reviewerID)

	entry, err := scanEntry(row)
	if err == nil {
		return entry, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Entry{}, err
	}
	return Entry{}, s.explainMiss(ctx, id, workspaceID)
}

// Archive 归档一条**已审**条目（已批准或已拒绝）。
//
// 刻意不允许 pending → archived：那是一条绕开审批的路。待审条目不想要了就是
// 拒绝，不是归档——两者的区别是「有人看过并否决了」与「没人看过」。
func (s *Store) Archive(ctx context.Context, id, workspaceID pgtype.UUID, note, reviewerType string, reviewerID pgtype.UUID) (Entry, error) {
	if !ValidReviewerType(reviewerType) {
		return Entry{}, fmt.Errorf("%w: 审阅人类型 %q", ErrUnknownStatus, reviewerType)
	}
	if !reviewerID.Valid {
		return Entry{}, errors.New("knowledge: 归档也要记是谁做的")
	}
	row := s.db.QueryRow(ctx, `
UPDATE ruel_project_knowledge
   SET status           = 'archived',
       review_note      = $3,
       reviewed_by_type = $4,
       reviewed_by_id   = $5,
       reviewed_at      = now(),
       updated_at       = now()
 WHERE id = $1 AND workspace_id = $2 AND status IN ('approved', 'rejected')
RETURNING `+entryColumns,
		id, workspaceID, note, reviewerType, reviewerID)

	entry, err := scanEntry(row)
	if err == nil {
		return entry, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Entry{}, err
	}
	return Entry{}, s.explainMiss(ctx, id, workspaceID)
}

// explainMiss 在 UPDATE 影响 0 行之后回读一次，把「没找到」「跨了 workspace」
// 「状态不对」三件事分开。
//
// 不分开的话三种情况都只会得到一句 ErrNoRows，调用方无法决定该回 404 还是 409——
// 而这两种响应对用户是完全不同的意思。
func (s *Store) explainMiss(ctx context.Context, id, workspaceID pgtype.UUID) error {
	var status string
	err := s.db.QueryRow(ctx, `
SELECT status FROM ruel_project_knowledge
 WHERE id = $1 AND workspace_id = $2`, id, workspaceID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		// 不存在，或者属于别的 workspace。两者对外都是「这里没有这条」——
		// 刻意不区分，否则会变成一个探测别的 workspace 有没有某条记录的接口。
		//
		// 用 %w 包住哨兵：调用方（handler）靠 errors.Is 决定回 404 还是 409，
		// 而不是靠比对这句提示语的文本。
		return fmt.Errorf("%w: %v", ErrNotFound, id)
	}
	if err != nil {
		return err
	}
	return ErrNotPending{Status: status}
}

// DeleteByWorkspace 删除某 workspace 的全部条目，返回删除行数。
//
// 工作区拆除时调用（workspace.go 的 deleteSteps）。虽然 workspace_id 上的外键
// 带 CASCADE、删 workspace 行本身也会带走这些记录，仍然显式删一次：拆除图里
// 每一步都要能被读到，靠级联实现的清理在代码里是看不见的。
func (s *Store) DeleteByWorkspace(ctx context.Context, workspaceID pgtype.UUID) (int64, error) {
	if !workspaceID.Valid {
		return 0, errors.New("knowledge: workspace_id 必填")
	}
	tag, err := s.db.Exec(ctx, `
DELETE FROM ruel_project_knowledge WHERE workspace_id = $1`, workspaceID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func scanEntry(row pgx.Row) (Entry, error) {
	var e Entry
	err := row.Scan(
		&e.ID, &e.WorkspaceID, &e.Statement, &e.Rationale,
		&e.SourceTaskID, &e.SourceIssueID, &e.SourceCommentID,
		&e.Status, &e.ReviewNote, &e.ReviewedByType, &e.ReviewedByID, &e.ReviewedAt,
		&e.CreatedAt, &e.UpdatedAt,
	)
	if err != nil {
		return Entry{}, err
	}
	return e, nil
}

func scanEntries(rows pgx.Rows) ([]Entry, error) {
	var out []Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
