package main

// Ruel 新增：两个「不用算账单也能看见」的命令。
//
//	multica usage-completeness   用量行的模型名采没采到（#34）
//	multica gate-refusals        委派链闸门的拒绝回执（#33）
//
// 两条命令的共同点就是它们存在的理由：各自要盯的那件事**在库里不留痕迹**。
//
//   - 模型名漏采：用量行照样写进去了，只是 `model` 是个占位值。要发现它，过去只能靠有人
//     去算账单（#32 就是这么撞见的）。
//   - 闸门拒绝：拒绝的方式是返回 error，调用方随即回滚事务。事务一回滚，库里什么都不剩。
//
// 所以两者都做成**进程内累计 + 问一句就有**，而不是再去库里找——库里没有。

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
	"github.com/spf13/cobra"
)

// ---------- usage-completeness ----------

var usageCompletenessCmd = &cobra.Command{
	Use:   "usage-completeness",
	Short: "Show how often usage rows are recorded without a model name",
	Long: `Report, per provider, how many usage rows landed and how many of them carry a
placeholder model name instead of a real one.

A row with a placeholder model cannot be priced, and it looks exactly like a row
the pricing table simply does not know — the difference is that the first one is a
collection defect you can fix. This command is the difference made visible without
having to add up a bill first (#34).

Counts accumulate in the running server process, so they reset when it restarts.
They answer "is this process dropping model names right now"; for "how many
historical rows are affected" query task_usage.model instead — that is a different
question with a different answer.

Requires the API server.`,
	RunE: runUsageCompleteness,
}

func init() {
	usageCompletenessCmd.Flags().String("output", "table", "Output format: table or json")
	rootCmd.AddCommand(usageCompletenessCmd)
}

func runUsageCompleteness(cmd *cobra.Command, _ []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	output, _ := cmd.Flags().GetString("output")

	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	var report usageCompletenessReport
	if err := client.GetJSON(ctx, "/api/usage-completeness", &report); err != nil {
		return fmt.Errorf("get usage completeness: %w", err)
	}

	if output == "json" {
		return cli.PrintJSON(os.Stdout, report)
	}
	printUsageCompletenessTable(os.Stdout, report)
	return nil
}

// usageCompletenessReport 与 /api/usage-completeness 的 JSON 形状对齐。
type usageCompletenessReport struct {
	GeneratedAt time.Time                    `json:"generated_at"`
	Providers   []usageCompletenessProvider `json:"providers"`
	Totals      usageCompletenessProvider   `json:"totals"`
}

type usageCompletenessProvider struct {
	Provider    string `json:"provider"`
	Rows        int64  `json:"rows"`
	MissingRows int64  `json:"missing_rows"`
}

func printUsageCompletenessTable(out io.Writer, report usageCompletenessReport) {
	fmt.Fprintf(out, "用量行模型名采集完整性 — 不用算账单也能看见（#34）\n")
	fmt.Fprintf(out, "生成时间: %s\n\n", report.GeneratedAt.Format(time.RFC3339))

	if len(report.Providers) == 0 {
		fmt.Fprintf(out, "本进程还没有见到任何用量行。\n")
		fmt.Fprintf(out, "（累计是进程内的，服务重启即清零；没有用量行 ≠ 没有漏采。）\n")
		return
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "provider\t用量行\t缺模型名\t缺失占比")
	for _, p := range report.Providers {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%s\n",
			providerLabel(p.Provider), p.Rows, p.MissingRows, missingRatioLabel(p.Rows, p.MissingRows))
	}
	fmt.Fprintf(tw, "合计\t%d\t%d\t%s\n",
		report.Totals.Rows, report.Totals.MissingRows,
		missingRatioLabel(report.Totals.Rows, report.Totals.MissingRows))
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "usage-completeness: 渲染表格失败: %v\n", err)
	}

	fmt.Fprintf(out, "\n占比在样本极小时不可信（1/1 = 100%%），所以本命令只报数、不定阈值。\n")
	fmt.Fprintf(out, "出现 unpriced 时先假设是采集缺陷（PRD 6.8.1 第 5 条），这一列就是验证那个假设的地方。\n")
}

// missingRatioLabel 样本为 0 时写「—」而不是 0%。
//
// 0% 会被读成「这个 provider 采得很全」，而真相是「还没见过它的行」。
func missingRatioLabel(rows, missing int64) string {
	if rows <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f%%", float64(missing)/float64(rows)*100)
}

// providerLabel 空 provider 要显式标出来。留空会被读成「这一行没有 provider」，
// 而真相是「这条用量行连 provider 都没采到」——那是比模型名缺失更严重的漏采。
func providerLabel(provider string) string {
	if provider == "" {
		return "(空)"
	}
	return provider
}

// ---------- gate-refusals ----------

var gateRefusalsCmd = &cobra.Command{
	Use:   "gate-refusals",
	Short: "Show delegation-gate refusals that left no trace in the database",
	Long: `Report every refusal of the delegation depth / cost-budget gates that this
machine has recorded.

These refusals are invisible by construction: the gate returns an error, the caller
rolls the transaction back, and nothing survives to be queried. The receipt is kept
outside the transaction on purpose (#33).

Refusals are recorded by whichever process refused — comment-triggered delegation
runs in the API server, wakeup-triggered delegation runs in the daemon. This command
asks both and labels which side each row came from.

Counts live in process memory and reset on restart.

The report also includes the webhook push channel (#36): whether it is configured,
how many refusals were delivered, and how many were dropped after exhausting their
retries. An unconfigured channel is reported as such rather than omitted — a push
channel that fails silently is worse than no channel at all.`,
	RunE: runGateRefusals,
}

func init() {
	gateRefusalsCmd.Flags().String("output", "table", "Output format: table or json")
	rootCmd.AddCommand(gateRefusalsCmd)
}

func runGateRefusals(cmd *cobra.Command, _ []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	output, _ := cmd.Flags().GetString("output")

	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	// API 侧：评论触发的委派在这里被拒。
	var apiReport gateRefusalsReport
	if err := client.GetJSON(ctx, "/api/gate-refusals", &apiReport); err != nil {
		return fmt.Errorf("get gate refusals: %w", err)
	}
	apiReport.Source = "api"

	// daemon 侧：wakeup 触发的委派在这里被拒。daemon 不在就跳过——不能因为一个进程没起
	// 就把另一边的回执也报成失败。
	var reports []gateRefusalsReport
	reports = append(reports, apiReport)
	profile := resolveProfile(cmd)
	port := healthPortForProfile(profile)
	if health := checkDaemonHealthOnPort(ctx, port); daemonAlive(health) {
		var daemonReport gateRefusalsReport
		if err := fetchJSONFromDaemon(ctx, port, "/gate-refusals", &daemonReport); err == nil {
			daemonReport.Source = "daemon"
			reports = append(reports, daemonReport)
		} else {
			fmt.Fprintf(os.Stderr, "gate-refusals: 读取 daemon 侧失败（%v），只显示 API 侧\n", err)
		}
	} else {
		fmt.Fprintf(os.Stderr, "gate-refusals: daemon 未运行（端口 %d），只显示 API 侧\n", port)
	}

	if output == "json" {
		return cli.PrintJSON(os.Stdout, reports)
	}
	printGateRefusalsTable(os.Stdout, reports)
	return nil
}

// gateRefusalsReport 与两个端点的 JSON 形状对齐。
type gateRefusalsReport struct {
	Source      string             `json:"source,omitempty"`
	GeneratedAt time.Time          `json:"generated_at"`
	Process     string             `json:"process"`
	Notices     []gateRefusalEntry `json:"notices"`
	Push        gateRefusalsPush   `json:"push"`
}

// gateRefusalsPush 是推送通道的自检面（#36）。
type gateRefusalsPush struct {
	Configured bool   `json:"configured"`
	URL        string `json:"url,omitempty"`
	Process    string `json:"process,omitempty"`
	Pending    int    `json:"pending"`
	Delivered  int64  `json:"delivered"`
	Dropped    int64  `json:"dropped"`
	Attempts   int64  `json:"attempts"`
	LastError  string `json:"last_error,omitempty"`
	LastAt     string `json:"last_at,omitempty"`
}

type gateRefusalEntry struct {
	Dimension    string   `json:"dimension"`
	Path         string   `json:"path"`
	IssueID      string   `json:"issue_id"`
	AgentID      string   `json:"agent_id"`
	Depth        *int     `json:"depth,omitempty"`
	DepthLimit   *int     `json:"depth_limit,omitempty"`
	SpentUSD     *float64 `json:"spent_usd,omitempty"`
	BudgetUSD    *float64 `json:"budget_usd,omitempty"`
	PricedRows   *int     `json:"priced_rows,omitempty"`
	UnpricedRows *int     `json:"unpriced_rows,omitempty"`
	LastAt       time.Time `json:"last_at"`
	Count        int      `json:"count"`
}

func fetchJSONFromDaemon(ctx context.Context, port int, path string, out any) error {
	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil,
	)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon 返回 %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func printGateRefusalsTable(out io.Writer, reports []gateRefusalsReport) {
	fmt.Fprintf(out, "闸门拒绝回执 — 事务已回滚，库里不留痕迹（#33）\n\n")
	total := 0
	for _, r := range reports {
		total += len(r.Notices)
	}
	if total == 0 {
		fmt.Fprintf(out, "没有任何拒绝记录。\n")
		fmt.Fprintf(out, "注意：累计是进程内的，服务重启即清零；且只含**当前运行的进程**见到的拒绝。\n")
		printGatePushStatus(out, reports)
		return
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "来源\t维度\tIssue\t入口\t次数\t最近\t详情")
	for _, r := range reports {
		for _, n := range r.Notices {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
				r.Source,
				gateRefusalDimensionLabel(n.Dimension),
				shortUUID(n.IssueID),
				n.Path,
				n.Count,
				n.LastAt.Format("15:04:05"),
				gateRefusalDetail(n),
			)
		}
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "gate-refusals: 渲染表格失败: %v\n", err)
	}
	fmt.Fprintf(out, "\n深度耗尽可以换一条链重来；预算是 per_issue 累计，不会自己归零。\n")
	printGatePushStatus(out, reports)
}

// printGatePushStatus 打印推送通道的自检面。
//
// 这一段必须**在没有拒绝时也照常打印**：「未配置」本身是一个要让人看见的答案。
// 一个配错 URL 却什么都不显示的推送通道，比没有通道更糟——它让人以为有人在看着。
func printGatePushStatus(out io.Writer, reports []gateRefusalsReport) {
	fmt.Fprintf(out, "\n推送通道（#36）\n")
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "来源\t状态\t目标\t待投\t已投\t丢弃\t最后一次错误")
	for _, r := range reports {
		state := "未配置"
		if r.Push.Configured {
			state = "已配置"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%s\n",
			r.Source, state, pushTargetLabel(r.Push),
			r.Push.Pending, r.Push.Delivered, r.Push.Dropped, pushErrorLabel(r.Push))
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "gate-refusals: 渲染推送状态失败: %v\n", err)
	}
	fmt.Fprintf(out, "未配置时不起 goroutine、不产生流量，回执端点照常工作。\n")
	fmt.Fprintf(out, "丢弃 = 重试到上限后放弃；进程重启会丢掉没投出去的条目，这是有意的取舍。\n")
}

func pushTargetLabel(p gateRefusalsPush) string {
	if !p.Configured || p.URL == "" {
		return "—"
	}
	return p.URL
}

func pushErrorLabel(p gateRefusalsPush) string {
	if p.LastError == "" {
		return "—"
	}
	return p.LastError
}

// gateRefusalDimensionLabel 两个维度在表里要能一眼分开：它们的出路不一样。
func gateRefusalDimensionLabel(dimension string) string {
	switch dimension {
	case "delegation_depth_exceeded":
		return "深度"
	case "delegation_budget_exceeded":
		return "预算"
	default:
		return dimension
	}
}

// gateRefusalDetail 把「该知道的数字」全写出来。
//
// 预算那条必须带上算不出来的行数：那决定了「已花」是个实际值还是个下限。藏掉它，人会把
// 一个下限读成实际值。
func gateRefusalDetail(n gateRefusalEntry) string {
	if n.Depth != nil && n.DepthLimit != nil {
		return fmt.Sprintf("深度 %d / 上限 %d", *n.Depth, *n.DepthLimit)
	}
	if n.SpentUSD != nil && n.BudgetUSD != nil {
		return fmt.Sprintf("已花 $%.4f / 上限 $%.4f；%d 行计价、%d 行算不出来",
			*n.SpentUSD, *n.BudgetUSD, derefInt(n.PricedRows), derefInt(n.UnpricedRows))
	}
	return "—"
}

func derefInt(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func shortUUID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
