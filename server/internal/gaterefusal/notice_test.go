package gaterefusal

// Ruel 新增：#33 —— 闸门拒绝的事务外回执。
//
// 四条纪律，各有用例：
//  1. 同一个 (Issue, 维度) 在 2 秒内重复被拒只留一条——通知自己不能变成噪音。
//  2. 不同维度 / 不同 Issue 不合并——它们的出路不同，合成一条就说不清该怎么修。
//  3. 合并时保留第一次的 Path：第一次被拒的入口才是那条链跑歪的起点。
//  4. 条数有上限：这是「最近发生了什么」的窗口，不是审计日志。

import (
	"fmt"
	"testing"
	"time"
)

func notice(issue, dimension, path string) Notice {
	return Notice{IssueID: issue, Dimension: dimension, Path: path}
}

func TestRecordMergesRepeatsInsideTheWindow(t *testing.T) {
	Reset()
	defer Reset()

	Record(notice("issue-1", DimensionBudget, "enqueue_issue_task"))
	Record(notice("issue-1", DimensionBudget, "enqueue_issue_task"))
	Record(notice("issue-1", DimensionBudget, "enqueue_issue_task"))

	got := Snapshot()
	if len(got) != 1 {
		t.Fatalf("回执条数 = %d, want 1（2 秒内的重复要合并，否则通知自己就成了噪音）", len(got))
	}
	if got[0].Count != 3 {
		t.Errorf("Count = %d, want 3", got[0].Count)
	}
}

func TestRecordKeepsDimensionsApart(t *testing.T) {
	Reset()
	defer Reset()

	Record(notice("issue-1", DimensionDepth, "enqueue_issue_task"))
	Record(notice("issue-1", DimensionBudget, "enqueue_issue_task"))

	if got := Snapshot(); len(got) != 2 {
		t.Fatalf("回执条数 = %d, want 2（两个维度出路不同，不能合并）", len(got))
	}
}

func TestRecordKeepsIssuesApart(t *testing.T) {
	Reset()
	defer Reset()

	Record(notice("issue-1", DimensionBudget, "enqueue_issue_task"))
	Record(notice("issue-2", DimensionBudget, "enqueue_issue_task"))

	if got := Snapshot(); len(got) != 2 {
		t.Fatalf("回执条数 = %d, want 2（不同 Issue 是两条链）", len(got))
	}
}

// TestRecordSplitsOutsideTheWindow 守的是「合并有时间边界」。
//
// 只合并 2 秒内的：委派链可以在几分钟里被反复触发，那些是**不同时刻的拒绝**，合成一条
// 会把「一直在发生」说成「发生过一次」。
func TestRecordSplitsOutsideTheWindow(t *testing.T) {
	Reset()
	defer Reset()

	Record(notice("issue-1", DimensionBudget, "enqueue_issue_task"))
	// 把这条回执人为变旧，模拟「上一次拒绝发生在 2 秒之前」。Record 每次都会把 LastAt
	// 写成当前时间，所以只能在这里改已经存下来的那条。
	mu.Lock()
	notices[0].LastAt = time.Now().UTC().Add(-DedupeWindow - time.Second)
	mu.Unlock()
	Record(notice("issue-1", DimensionBudget, "enqueue_issue_task"))

	got := Snapshot()
	if len(got) != 2 {
		t.Fatalf("回执条数 = %d, want 2（超出去重窗口要另起一条）", len(got))
	}
}

// TestRecordKeepsTheFirstPath 守的是合并时**不覆盖** Path。
//
// 入口变了本身是个信号，但排查一条跑歪的链时要看的是它从哪条路转进来的第一次。
func TestRecordKeepsTheFirstPath(t *testing.T) {
	Reset()
	defer Reset()

	Record(notice("issue-1", DimensionBudget, "enqueue_issue_task"))
	Record(notice("issue-1", DimensionBudget, "issue_wakeup"))

	got := Snapshot()
	if len(got) != 1 {
		t.Fatalf("回执条数 = %d, want 1", len(got))
	}
	if got[0].Path != "enqueue_issue_task" {
		t.Errorf("Path = %q, want %q（合并时保留第一次被拒的入口）", got[0].Path, "enqueue_issue_task")
	}
}

// TestRecordRefreshesNumbersButNotIdentity 守的是「数字刷新为最新，身份不变」。
func TestRecordRefreshesNumbersButNotIdentity(t *testing.T) {
	Reset()
	defer Reset()

	spent := 1.80
	budget := 1.7654
	rows := 3
	unpriced := 0
	Record(Notice{
		IssueID: "issue-1", Dimension: DimensionBudget, Path: "enqueue_issue_task",
		SpentUSD: &spent, BudgetUSD: &budget, PricedRows: &rows, UnpricedRows: &unpriced,
	})
	spent2 := 2.40
	rows2 := 4
	Record(Notice{
		IssueID: "issue-1", Dimension: DimensionBudget, Path: "enqueue_issue_task",
		SpentUSD: &spent2, PricedRows: &rows2,
	})

	got := Snapshot()
	if len(got) != 1 {
		t.Fatalf("回执条数 = %d, want 1", len(got))
	}
	if got[0].SpentUSD == nil || *got[0].SpentUSD != 2.40 {
		t.Errorf("已花 = %v, want 2.40（数字要刷新为最近一次）", got[0].SpentUSD)
	}
	if got[0].PricedRows == nil || *got[0].PricedRows != 4 {
		t.Errorf("计价行数 = %v, want 4", got[0].PricedRows)
	}
	// 上限与「算不出来的行数」第二次没带，应当保留第一次的而不是被清空。
	if got[0].BudgetUSD == nil || *got[0].BudgetUSD != 1.7654 {
		t.Errorf("上限 = %v, want 1.7654（不该被后来的记录清掉）", got[0].BudgetUSD)
	}
}

func TestSnapshotIsSortedNewestFirst(t *testing.T) {
	Reset()
	defer Reset()

	Record(notice("issue-1", DimensionDepth, "p1"))
	Record(notice("issue-2", DimensionBudget, "p2"))

	got := Snapshot()
	if len(got) != 2 {
		t.Fatalf("回执条数 = %d, want 2", len(got))
	}
	if !got[0].LastAt.After(got[1].LastAt) && !got[0].LastAt.Equal(got[1].LastAt) {
		t.Errorf("快照未按最近发生排序: [%v, %v]", got[0].LastAt, got[1].LastAt)
	}
}

// TestSnapshotReturnsCopies 守的是「外部改不动内部状态」。
func TestSnapshotReturnsCopies(t *testing.T) {
	Reset()
	defer Reset()

	Record(notice("issue-1", DimensionBudget, "p1"))
	got := Snapshot()
	got[0].Count = 999
	if Snapshot()[0].Count != 1 {
		t.Error("Snapshot 返回的是引用，调用方能改内部状态")
	}
}

// TestRecordBoundsTheLog 守的是条数上限：这是「最近发生了什么」的窗口，不是审计日志。
func TestRecordBoundsTheLog(t *testing.T) {
	Reset()
	defer Reset()

	// 每条换一个 Issue，否则会被去重合并成一条。
	for i := 0; i < maxNotices+50; i++ {
		Record(notice(fmt.Sprintf("issue-%d", i), DimensionDepth, "p"))
	}
	if got := Snapshot(); len(got) > maxNotices {
		t.Errorf("回执条数 = %d, want <= %d", len(got), maxNotices)
	}
}
