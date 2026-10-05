package main

// Ruel 新增：清理预览（P0-9 的「清理有预览」）。
//
// 上游的清理是 daemon 后台自动 GC——CLI 里连清理命令都没有，清理自动发生，用户既不
// 能触发也不能预知。而 GC 一轮会删掉整个任务目录（agent 的工作区，未提交的工作一起
// 没）。本命令回答「下一轮会删哪些、为什么」。
//
// 刻意新建文件而不是并进 cmd_daemon.go：将来同步上游时这个文件不会与上游改动打架。
//
// 计算放在 daemon 侧（/gc-plan 端点），这里只负责取回并渲染。理由见 internal/daemon/
// gcplan.go：决策要用 daemon 的内存状态（哪些目录正在跑），CLI 另算一套会与真实 GC
// 分叉——**会撒谎的预览比没有预览更糟**。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/spf13/cobra"
)

var daemonGCPlanCmd = &cobra.Command{
	Use:   "gc-plan",
	Short: "Show what the next GC cycle would do, without doing it",
	Long: `Report what the daemon's next garbage-collection cycle would do to each task
directory — keep it, trim its regenerable artifacts, or remove it entirely.

Nothing is deleted. The daemon computes the preview using the same traversal and
the same decisions the real GC uses, so the answer cannot drift from what will
actually happen.

Scope: task directories only. The bare-repo cache, codex/hermes session stores
and temp directories are reclaimed on their own schedules and are not previewed.

Requires a running daemon: it is the only thing that knows which directories are
in use right now.`,
	RunE: runDaemonGCPlan,
}

func init() {
	daemonGCPlanCmd.Flags().String("output", "table", "Output format: table or json")
	daemonCmd.AddCommand(daemonGCPlanCmd)
}

func runDaemonGCPlan(cmd *cobra.Command, _ []string) error {
	profile := resolveProfile(cmd)
	port := healthPortForProfile(profile)
	output, _ := cmd.Flags().GetString("output")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 先探活。daemon 不在时必须明确报错——返回一个空列表会被读成「没有要清理的」，
	// 而真相是「算不出来」，这两者天差地别。
	if health := checkDaemonHealthOnPort(ctx, port); !daemonAlive(health) {
		return fmt.Errorf(
			"daemon 未运行（端口 %d 无应答），无法生成清理预览。\n"+
				"预览由 daemon 自己计算：它才知道哪些目录正在跑，CLI 另算一套会与真实 GC 分叉。\n"+
				"请先执行 multica daemon start",
			port,
		)
	}

	report, err := fetchGCPlan(ctx, port)
	if err != nil {
		return err
	}

	if output == "json" {
		return cli.PrintJSON(os.Stdout, report)
	}
	printGCPlanTable(os.Stdout, report)
	return nil
}

func fetchGCPlan(ctx context.Context, port int) (daemon.GCPlanReport, error) {
	var report daemon.GCPlanReport
	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/gc-plan", port), nil,
	)
	if err != nil {
		return report, err
	}
	resp, err := (&http.Client{Timeout: 25 * time.Second}).Do(req)
	if err != nil {
		return report, fmt.Errorf("gc-plan: 请求 daemon 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return report, fmt.Errorf("gc-plan: daemon 返回 %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
		return report, fmt.Errorf("gc-plan: 解析响应失败: %w", err)
	}
	return report, nil
}

func printGCPlanTable(out io.Writer, report daemon.GCPlanReport) {
	fmt.Fprintf(out, "GC 预览 — 下一轮会做什么（不会真的做）\n")
	fmt.Fprintf(out, "根目录: %s\n", report.WorkspacesRoot)
	fmt.Fprintf(out, "范围:   %s\n\n", report.Scope)

	if len(report.Entries) == 0 {
		fmt.Fprintln(out, "没有任务目录可预览。")
		return
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ACTION\tKIND\tTASK\tAGE\tSIZE\tRECLAIM\tREASON")
	for _, entry := range report.Entries {
		fmt.Fprintf(
			tw,
			"%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			entry.Action,
			entry.Kind,
			entry.TaskShort,
			formatAge(entry.AgeSeconds),
			formatBytes(entry.SizeBytes),
			gcPlanReclaim(entry),
			entry.Reason,
		)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "gc-plan: 渲染表格失败: %v\n", err)
	}

	fmt.Fprintf(
		out,
		"\n合计 %d 个任务目录：将整目录删除 %d 个（%s），只清产物 %d 个（%s），正在运行 %d 个，保留 %d 个\n",
		report.TotalTaskDirs,
		report.WillRemoveDirs,
		formatBytes(report.WillRemoveBytes),
		report.WillTrimArtifacts,
		formatBytes(report.WillTrimArtifactBytes),
		report.ActiveDirs,
		report.TotalTaskDirs-report.WillRemoveDirs-report.WillTrimArtifacts-report.ActiveDirs,
	)
	if report.WillRemoveDirs > 0 {
		// 这句要说出来：被删的是工作区，未提交的工作不在任何远端。
		fmt.Fprintln(out, "注意：整目录删除会连未提交的工作一起删掉。")
	}
}

func gcPlanReclaim(entry daemon.GCPlanEntry) string {
	if entry.RemovableBytes <= 0 {
		return "-"
	}
	// 只清产物的两条用的是「可再生产物子集」的大小，是上限而非精确值——标出来，
	// 免得被当成精确承诺。
	if strings.HasPrefix(entry.Action, "clean_artifacts") ||
		strings.HasPrefix(entry.Action, "clean_managed_artifacts") {
		return "≤" + formatBytes(entry.RemovableBytes)
	}
	return formatBytes(entry.RemovableBytes)
}
