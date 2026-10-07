package main

// Ruel 新增：四类历史的保留策略（P0-9 的另一半）。
//
// 与 gc-plan 的关系必须说清，因为两条命令长得最像、也最容易合并：
//
//	gc-plan     回答「下一轮会删什么」——是对即将发生的事的预测
//	gc-policy   回答「各类东西能活多久」——是当前配置的快照
//
// 刻意做成两个命令。合并后用户得分清哪一列是预测、哪一列是配置，而这两列恰好最像。
// 一个问题一个答案。
//
// 计算放在 daemon 侧，理由比 gc-plan 那条更硬：这里要的是**生效值**，而生效值只有
// daemon 装载完 cfg 之后才存在。CLI 侧照着常量表拼一遍，展示的就是默认值——用户照着
// 它判断「我的东西还能活多久」，拿到的是另一套配置。**展示默认值等于撒谎。**

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"text/tabwriter"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/spf13/cobra"
)

var daemonGCPolicyCmd = &cobra.Command{
	Use:   "gc-policy",
	Short: "Show each history class's retention policy and current footprint",
	Long: `Report what the daemon keeps, for how long, and how much each class takes
right now — task directories, CLI sessions, approved knowledge, caches, and
server-side records.

Every retention shown is the value the running daemon is actually using, not the
compiled-in default: all of them can be overridden by MULTICA_GC_* environment
variables, and a report built from defaults would describe a configuration that
is not in effect.

This answers "how long can each kind of thing live". For "what would the next
cycle delete" use gc-plan instead — the two are deliberately separate.

Requires a running daemon: the effective configuration only exists there.`,
	RunE: runDaemonGCPolicy,
}

func init() {
	daemonGCPolicyCmd.Flags().String("output", "table", "Output format: table or json")
	daemonCmd.AddCommand(daemonGCPolicyCmd)
}

func runDaemonGCPolicy(cmd *cobra.Command, _ []string) error {
	profile := resolveProfile(cmd)
	port := healthPortForProfile(profile)
	output, _ := cmd.Flags().GetString("output")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 先探活，理由同 gc-plan：daemon 不在时返回一个「全是默认值」的表，会被读成
	// 「配置就是这样」，而真相是「读不到」。
	if health := checkDaemonHealthOnPort(ctx, port); !daemonAlive(health) {
		return fmt.Errorf(
			"daemon 未运行（端口 %d 无应答），无法读取保留策略。\n"+
				"本命令展示的是 daemon 装载后的**生效值**；daemon 不在就没有生效值，\n"+
				"按默认值拼一份等于拿默认值冒充你的配置。\n"+
				"请先执行 multica daemon start",
			port,
		)
	}

	report, err := fetchGCPolicy(ctx, port)
	if err != nil {
		return err
	}

	if output == "json" {
		return cli.PrintJSON(os.Stdout, report)
	}
	printGCPolicyTable(os.Stdout, report)
	return nil
}

func fetchGCPolicy(ctx context.Context, port int) (daemon.GCPolicyReport, error) {
	var report daemon.GCPolicyReport
	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/gc-policy", port), nil,
	)
	if err != nil {
		return report, err
	}
	resp, err := (&http.Client{Timeout: 25 * time.Second}).Do(req)
	if err != nil {
		return report, fmt.Errorf("gc-policy: 请求 daemon 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return report, fmt.Errorf("gc-policy: daemon 返回 %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
		return report, fmt.Errorf("gc-policy: 解析响应失败: %w", err)
	}
	return report, nil
}

func printGCPolicyTable(out io.Writer, report daemon.GCPolicyReport) {
	fmt.Fprintf(out, "保留策略 — 各类历史能活多久（生效值，非默认值）\n")
	fmt.Fprintf(out, "生成时间: %s\n", report.GeneratedAt.Format(time.RFC3339))
	if report.Profile != "" {
		fmt.Fprintf(out, "profile:  %s\n", report.Profile)
	}
	if !report.GCEnabled {
		fmt.Fprintf(out, "GC:       **已关闭**（%s）—— 以下 TTL 全部不生效\n", report.GCEnabledVar)
	} else {
		fmt.Fprintf(out, "GC:       开启，每 %s 跑一轮\n", report.GCInterval)
	}
	fmt.Fprintf(out, "范围:     %s\n\n", report.Scope)

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "类别\t能清吗\t保留多久\t来源\t占用\t条目\t受保护")
	for _, e := range report.Entries {
		fmt.Fprintf(
			tw,
			"%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			e.Label,
			gcPolicyReclaimable(e),
			gcPolicyRetention(e),
			gcPolicySource(e),
			gcPolicySize(e),
			gcPolicyCount(e),
			gcPolicyProtected(e),
		)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "gc-policy: 渲染表格失败: %v\n", err)
	}

	fmt.Fprintf(out, "\n可统计部分合计 %s\n", formatBytes(report.TotalBytes))
	for _, e := range report.Entries {
		if e.Note != "" {
			fmt.Fprintf(out, "· %s：%s\n", e.Label, e.Note)
		}
	}
	fmt.Fprintf(out, "\n要看「下一轮会删什么」用 gc-plan；本命令只回答「各类能活多久」。\n")
}

// gcPolicyReclaimable 把「能不能被清」说准：显式禁用与「这一类根本没有策略」是两回事。
func gcPolicyReclaimable(e daemon.GCPolicyEntry) string {
	if !e.Measurable {
		return "本机不知"
	}
	if e.Disabled {
		return "已禁用"
	}
	if e.Reclaimable {
		return "能"
	}
	return "不能"
}

func gcPolicyRetention(e daemon.GCPolicyEntry) string {
	if e.Retention == "" {
		return "—"
	}
	return e.Retention
}

// gcPolicySource 标出这一行的保留时长是**默认值还是被覆盖过**。这是本命令存在的理由：
// 没有这一列，用户无法判断展示的数字是不是自己那台机器的。
func gcPolicySource(e daemon.GCPolicyEntry) string {
	if e.EnvVar == "" {
		return "无开关"
	}
	if e.Overridden {
		return "环境变量"
	}
	return "默认值"
}

// gcPolicySize 量不出来时写「未知」而不是 0。0 会被读成「这块是空的」。
func gcPolicySize(e daemon.GCPolicyEntry) string {
	if !e.Measurable {
		return "不在本机"
	}
	if !e.SizeKnown {
		return "未知"
	}
	return formatBytes(e.SizeBytes)
}

func gcPolicyCount(e daemon.GCPolicyEntry) string {
	if !e.Measurable || !e.SizeKnown {
		return "—"
	}
	return fmt.Sprintf("%d", e.ItemCount)
}

// gcPolicyProtected 受保护的条目要显式标出来。标了「0」也比留空好：留空会被读成
// 「这一类随时可能被删」，而正在跑的任务目录恰恰是不会被删的。
func gcPolicyProtected(e daemon.GCPolicyEntry) string {
	if !e.Measurable {
		return "—"
	}
	if e.Protected > 0 {
		return fmt.Sprintf("%d（不会被删）", e.Protected)
	}
	if e.Reclaimable && !e.Disabled {
		return "0"
	}
	return "—"
}
