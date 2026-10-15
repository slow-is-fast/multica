// Package summary 实现压缩层（PRD 第 5 章 5.5 节）：Run 结束时从已交付评论里提取
// agent 写的结构化总结块，把「决定」落成待审批的项目知识条目。
//
// 设计约束见 ruel/docs/reference/multica-summary-producer-and-format.md（#46 结论）：
//   - 块由 agent 在最终评论末尾以保留 info string 的围栏块给出，服务端只解析它，
//     永不解析块外的散文。
//   - 块必须同时满足 version 已知 + 可 unmarshal + round-trip 一致 才采信；否则
//     判定为不可用，不产生任何条目（#47 原则：没有决定就没有行，而不是插一条空壳）。
//   - 只把 decisions 落成知识条目；open_items 是待办不是知识，不进知识库。
//   - 触发点基于「已确认的交付游标」：本实现锚定 source_task_id = 当前 task，该字段
//     由平台在评论创建时按 X-Task-ID 头盖章，是「这条评论属于哪个 Run」的权威标识，
//     比 task 终态更可靠——避开了 #46 §3.4 的事务边界陷阱。
//
// 沿用 knowledge / artifacts 包的手写查询约定（不依赖 sqlc 生成物），把改动圈在本
// 包内，将来同步上游时一眼能看出哪些是我们加的。
package summary

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/ruel/knowledge"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// DBTX 与 knowledge / artifacts 同形状：pgx 最小子集，与 handler 的 dbExecutor 一致。
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// blockInfoString 是 agent 在评论里使用的围栏块 info string（#46 提议、#50 首次落地）。
const blockInfoString = "multica-run-summary"

// fencedBlockRE 抓取形如 ```multica-run-summary\n...\n``` 的围栏块。
//
// 只认精确等于 blockInfoString 的 info string：既不会误吞 `html` / `mermaid` 这类
// 渲染块，也因为非贪婪匹配 + 要求以专属 info string 开头，保证抓到的是我们自己的块。
var fencedBlockRE = regexp.MustCompile("(?s)```" + regexp.QuoteMeta(blockInfoString) + "\n(.*?)```")

// runSummary 对应 multica-run-summary 块的 JSON 主体。
type runSummary struct {
	Version   int        `json:"version"`
	Decisions []decision `json:"decisions"`
	OpenItems []openItem `json:"open_items"`
}

type decision struct {
	What string   `json:"what"`
	Why  string   `json:"why"`
	Refs []string `json:"refs"`
}

type openItem struct {
	What       string `json:"what"`
	WhyLeft    string `json:"why_left"`
	BlockerRef string `json:"blocker_ref"`
}

// ExtractRunSummary 从一条评论正文里提取并校验 multica-run-summary 块。
//
// 返回 (summary, ok, reason)。ok=false 时 reason ∈ {no_block, malformed,
// version_unknown, roundtrip_mismatch}，对应 #46 §3.2 的四种不可用原因。调用方据此
// 决定不产出条目，但绝不能把「不可用」渲染成「无决定」。
func ExtractRunSummary(content string) (runSummary, bool, string) {
	raw := extractFencedBlock(content)
	if raw == "" {
		return runSummary{}, false, "no_block"
	}
	var s runSummary
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return runSummary{}, false, "malformed"
	}
	if s.Version != 1 {
		return runSummary{}, false, "version_unknown"
	}
	if !roundTripOK([]byte(raw), s) {
		return runSummary{}, false, "roundtrip_mismatch"
	}
	return s, true, ""
}

// roundTripOK 校验「规范化后一致」：把原文与重序列化结果都还原成 runSummary（未知字段
// 被两边一致地忽略），两者应逐字段相等。这证明我们没有在解析/序列化过程中丢失任何
// 建模字段——对应 #46 §3.3 照搬自 mergeWakeupEvidence 的 round-trip 门禁。
func roundTripOK(original []byte, s runSummary) bool {
	re, err := json.Marshal(s)
	if err != nil {
		return false
	}
	var fromOrig, fromRe runSummary
	if err := json.Unmarshal(original, &fromOrig); err != nil {
		return false
	}
	if err := json.Unmarshal(re, &fromRe); err != nil {
		return false
	}
	return reflect.DeepEqual(fromOrig, s) && reflect.DeepEqual(fromRe, s)
}

// extractFencedBlock 返回块内 JSON 文本（已去首尾空白），没有则返回空串。
func extractFencedBlock(content string) string {
	m := fencedBlockRE.FindStringSubmatch(content)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// runComment 是 listRunOutputComments 的轻量结果，只取建条目要用的列。
// workspaceID 直接取自评论文档行——它即是本条评论所属的工作区，作为知识条目的硬边界。
type runComment struct {
	id          pgtype.UUID
	content     string
	workspaceID pgtype.UUID
}

// listRunOutputComments 取出本 Run 由 agent 产出的评论（source_task_id = task.ID）。
//
// 锚点选 source_task_id 而非 delivered_comment_ids：后者是 claim 时「投递给 agent 的
// 输入」回执，不包含 agent 自己发出的结果评论（见 #46 §3.4 的更正）。source_task_id
// 由平台按 X-Task-ID 头在评论创建时盖章，唯一且权威。以 issue_id 二次约束，杜绝跨
// Issue 串到别的 Run 的评论。工作区取自评论行本身（agent_task_queue 上并无 workspace 列）。
func listRunOutputComments(ctx context.Context, dbtx DBTX, task *db.AgentTaskQueue) ([]runComment, error) {
	if !task.ID.Valid || !task.IssueID.Valid {
		return nil, nil
	}
	rows, err := dbtx.Query(ctx, `
SELECT id, content, workspace_id FROM comment
WHERE source_task_id = $1 AND issue_id = $2
ORDER BY created_at ASC, id ASC`, task.ID, task.IssueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []runComment
	for rows.Next() {
		var rc runComment
		if err := rows.Scan(&rc.id, &rc.content, &rc.workspaceID); err != nil {
			return nil, err
		}
		out = append(out, rc)
	}
	return out, rows.Err()
}

// decisionRationale 把一条决定的 why 与 refs 合并成知识条目的 rationale 文本。
//
// refs 并入 rationale（#50 设计点：不另开 jsonb 列），用「引用: ...」前缀，多个用逗号
// 分隔。why 为空时 rationale 只含引用，保证「有来源」这件事不会被丢掉。
func decisionRationale(d decision) string {
	rationale := strings.TrimSpace(d.Why)
	if len(d.Refs) == 0 {
		return rationale
	}
	joined := strings.Join(d.Refs, ", ")
	if rationale == "" {
		return "引用: " + joined
	}
	return rationale + " 引用: " + joined
}

// Produce 从本 Run 已交付的评论里提取总结块，把每条决定落成一条待审知识条目。
//
// 失败隔离原则：本函数本身只在「读不到评论 / 建条目失败」时返回 error，调用方（完成
// 回调）必须吞掉这个 error，绝不能让它冒泡成完成回调的失败——否则 daemon 会重试终态
// 回调，破坏「终态只落一次」的保证（P0-4）。
//
// 不可用（无块 / 损坏 / 版本未知 / round-trip 不符）⇒ 不产生任何条目、返回 nil：没有
// 决定就没有行（#47 原则），空壳占位比没有更糟，也更难排查。
func Produce(ctx context.Context, dbtx DBTX, task *db.AgentTaskQueue) error {
	if task == nil || !task.ID.Valid || !task.IssueID.Valid {
		return nil
	}
	comments, err := listRunOutputComments(ctx, dbtx, task)
	if err != nil {
		return fmt.Errorf("summary: 读取 Run 输出评论失败: %w", err)
	}

	var summary runSummary
	var foundCommentID pgtype.UUID
	var foundWorkspace pgtype.UUID
	var found bool
	var lastReason string
	for _, c := range comments {
		s, ok, reason := ExtractRunSummary(c.content)
		if ok {
			summary = s
			foundCommentID = c.id
			foundWorkspace = c.workspaceID
			found = true
			break
		}
		if reason != "" {
			lastReason = reason
		}
	}
	if !found {
		if lastReason != "" {
			slog.Warn("summary: 交付评论含损坏的 run-summary 块，已跳过（未生成条目）",
				"task_id", util.UUIDToString(task.ID), "reason", lastReason)
		}
		return nil
	}

	store := knowledge.NewStore(dbtx)
	for _, d := range summary.Decisions {
		what := strings.TrimSpace(d.What)
		if what == "" {
			// 决定没有实质内容：跳过这条，不插空壳。
			continue
		}
		if _, err := store.Create(ctx, knowledge.Entry{
			WorkspaceID:     foundWorkspace,
			Statement:       what,
			Rationale:       decisionRationale(d),
			SourceTaskID:    task.ID,
			SourceIssueID:   task.IssueID,
			SourceCommentID: foundCommentID,
		}); err != nil {
			return fmt.Errorf("summary: 创建知识条目失败: %w", err)
		}
	}
	return nil
}
