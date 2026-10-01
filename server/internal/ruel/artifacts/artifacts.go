// Package artifacts 提供 Run 产物的落点。
//
// 上游把 Run 结果塞进 agent_task_queue.result 这个 jsonb，只有 output / pr_url /
// work_dir / session_id 四项——没有 diff，也没有测试证据。P0-6 要求「Run 链接到隔离
// 分支、diff 与测试证据」，缺了这一层，验收就没有落脚点。本包补上。
//
// 刻意不依赖 sqlc 生成物：重新生成会大面积改动 pkg/db/generated，而那正是将来同步
// 上游时冲突最集中的地方。手写查询把改动圈在本包内，同步时一眼能看出哪些是我们加的。
package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// 产物类型。MVP 只用到前三种，test_log 留给后续接 CI 输出。
const (
	KindDiff       = "diff"
	KindDiffStat   = "diff_stat"
	KindFileChange = "file_change"
	KindTestLog    = "test_log"
)

// DBTX 是 pgx 的最小子集，形状与 handler 侧的 dbExecutor 一致。
type DBTX interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Artifact 是一条产物。checksum 必填——产物是验收的唯一证据，允许没有校验值的产物
// 等于让 Issue 页上那句「测试通过」失去可核对性。
type Artifact struct {
	ID          pgtype.UUID
	WorkspaceID pgtype.UUID
	TaskID      pgtype.UUID
	IssueID     pgtype.UUID
	Kind        string
	URI         string
	Size        int64
	Checksum    string
	Content     string
	CreatedAt   pgtype.Timestamptz
}

// Sum 计算内容的 sha256。
func Sum(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

// Store 是 ruel_artifacts 表的读写入口。
type Store struct{ db DBTX }

func NewStore(db DBTX) *Store { return &Store{db: db} }

// Upsert 写入一条产物。同一 Run 的同类产物覆盖写入，保留最新一份。
//
// checksum 由调用方用 Sum 计算后传入；若与 content 不符则拒绝入库——校验值的意义在于
// 事后可核对，客户端随手填一个也算「有值」，但那跟没有一样。
func (s *Store) Upsert(ctx context.Context, a Artifact) error {
	kind := strings.TrimSpace(a.Kind)
	if kind == "" {
		return fmt.Errorf("artifact: kind 不能为空")
	}
	if strings.TrimSpace(a.Checksum) == "" {
		return fmt.Errorf("artifact %s: checksum 必填", kind)
	}
	if a.Checksum != Sum(a.Content) {
		return fmt.Errorf("artifact %s: checksum 与内容不符", kind)
	}
	_, err := s.db.Exec(ctx, `
INSERT INTO ruel_artifacts (workspace_id, task_id, issue_id, kind, uri, size, checksum, content)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (task_id, kind) DO UPDATE SET
    workspace_id = EXCLUDED.workspace_id,
    issue_id     = EXCLUDED.issue_id,
    uri          = EXCLUDED.uri,
    size         = EXCLUDED.size,
    checksum     = EXCLUDED.checksum,
    content      = EXCLUDED.content,
    created_at   = now()`,
		a.WorkspaceID, a.TaskID, a.IssueID, kind, a.URI, int64(len(a.Content)), a.Checksum, a.Content)
	return err
}

// ListByTask 列出某次 Run 的全部产物。
func (s *Store) ListByTask(ctx context.Context, taskID pgtype.UUID) ([]Artifact, error) {
	rows, err := s.db.Query(ctx, `
SELECT id, workspace_id, task_id, issue_id, kind, uri, size, checksum, COALESCE(content, ''), created_at
  FROM ruel_artifacts
 WHERE task_id = $1
 ORDER BY kind`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAll(rows)
}

// ListByIssue 列出某个 Issue 下所有 Run 的产物。Issue 页要展示的是「这个需求总共改了
// 什么」，而不是某一次 Run 改了什么——多轮 Run 的变更要能一起看到。
func (s *Store) ListByIssue(ctx context.Context, issueID pgtype.UUID) ([]Artifact, error) {
	rows, err := s.db.Query(ctx, `
SELECT id, workspace_id, task_id, issue_id, kind, uri, size, checksum, COALESCE(content, ''), created_at
  FROM ruel_artifacts
 WHERE issue_id = $1
 ORDER BY created_at`, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAll(rows)
}

func scanAll(rows pgx.Rows) ([]Artifact, error) {
	var out []Artifact
	for rows.Next() {
		var a Artifact
		if err := rows.Scan(
			&a.ID, &a.WorkspaceID, &a.TaskID, &a.IssueID,
			&a.Kind, &a.URI, &a.Size, &a.Checksum, &a.Content, &a.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
