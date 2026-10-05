package main

// 预览的渲染也是预览的一部分：数字标错、警告不出现，用户照样会被误导。
// 这里只测渲染（纯函数，不需要 daemon）。

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon"
)

func TestPrintGCPlanTable_StatesScopeAndWarnsAboutWholeDirRemoval(t *testing.T) {
	report := daemon.GCPlanReport{
		GeneratedAt:    time.Unix(0, 0).UTC(),
		WorkspacesRoot: "/srv/workspaces",
		Scope:          "只覆盖任务目录",
		Entries: []daemon.GCPlanEntry{
			{
				WorkspaceShort: "ws0", TaskShort: "t-orphan", Kind: "issue",
				Action: daemon.GCPlanActionOrphan, Reason: "超过孤立 TTL",
				SizeBytes: 40 << 20, RemovableBytes: 40 << 20,
			},
			{
				WorkspaceShort: "ws0", TaskShort: "t-artifacts", Kind: "issue",
				Action: daemon.GCPlanActionCleanArtifacts, Reason: "只删可再生产物",
				SizeBytes: 100 << 20, ArtifactSizeBytes: 12 << 20, RemovableBytes: 12 << 20,
			},
			{
				WorkspaceShort: "ws0", TaskShort: "t-keep", Kind: "issue",
				Action: daemon.GCPlanActionKeep, Reason: "保留",
			},
		},
		TotalTaskDirs:         3,
		WillRemoveDirs:        1,
		WillRemoveBytes:       40 << 20,
		WillTrimArtifacts:     1,
		WillTrimArtifactBytes: 12 << 20,
		ActiveDirs:            0,
	}

	var buf bytes.Buffer
	printGCPlanTable(&buf, report)
	out := buf.String()

	for _, want := range []string{
		"只覆盖任务目录",   // 范围必须写出来：没列出来的容易被读成不会清理
		"orphan",    // 动作
		"超过孤立 TTL",  // 理由：为什么删
		"≤12.0 MiB", // 只清产物的是上限，不是精确值
		"未提交的工作",    // 整目录删除的代价必须说出口
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// 整目录删除是精确值，不能带 ≤。
	if strings.Contains(out, "≤40.0 MiB") {
		t.Errorf("whole-dir removal is exact, must not be prefixed with ≤:\n%s", out)
	}
}

func TestPrintGCPlanTable_EmptyPlanSaysSo(t *testing.T) {
	var buf bytes.Buffer
	printGCPlanTable(&buf, daemon.GCPlanReport{Scope: "只覆盖任务目录"})
	if got := buf.String(); !strings.Contains(got, "没有任务目录可预览") {
		t.Errorf("empty plan output = %q", got)
	}
	// 空计划也不能省掉范围，否则「什么都没列」会被读成「什么都不会被清理」。
	if got := buf.String(); !strings.Contains(got, "只覆盖任务目录") {
		t.Errorf("empty plan must still state scope: %q", got)
	}
}
