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
	"github.com/multica-ai/multica/server/internal/service"
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
	Notices       []gateRefusalEntry        `json:"notices"`
	Push          gateRefusalsPush          `json:"push"`
	// #40：per_run 成本上限。只有 API 进程那份带它——这道闸门跑在用量上报的收口上，
	// 而用量上报是 API 端点；daemon 侧从来没有这一层。
	PerRunBudget *gateRefusalsPerRunBudget `json:"per_run_budget,omitempty"`
	// #41：四个周期维度。
	PeriodBudget *gateRefusalsPeriodBudget `json:"period_budget,omitempty"`
}

// gateRefusalsPerRunBudget 与 service.RuelRunCostBudgetStatus 的 JSON 形状对齐。
type gateRefusalsPerRunBudget struct {
	Configured bool    `json:"configured"`
	BudgetUSD  float64 `json:"budget_usd,omitempty"`
	ParseError string  `json:"parse_error,omitempty"`
}

// gateRefusalsPeriodBudget 与 service.PeriodBudgetStatus 的 JSON 形状对齐。
type gateRefusalsPeriodBudget struct {
	At       string                        `json:"at"`
	Statuses []gateRefusalsPeriodDimension `json:"statuses"`
}

// 形状与 service.PeriodBudgetConfig 的 JSON **逐字段对齐**。
//
// 对齐不上会静默出错：Go 的 json 解码遇到类型不符会整个失败（dimension 是对象而这里
// 写成 string 时，整条命令只吐一句 unmarshal 错误），而不是少打印一列——所以这层不是
// 「差不多就行」，改一处就得改两处。
type gateRefusalsPeriodDimension struct {
	Dimension  gateRefusalsPeriodDimensionID `json:"dimension"`
	Key        string                        `json:"key"`
	EnvVar     string                        `json:"env_var"`
	Configured bool                          `json:"configured"`
	BudgetUSD  float64                       `json:"usd,omitempty"`
	ParseError string                        `json:"parse_error,omitempty"`
	Window     struct {
		Start string `json:"start"`
		End   string `json:"end"`
	} `json:"window"`
}

type gateRefusalsPeriodDimensionID struct {
	Scope  string `json:"scope"`
	Period string `json:"period"`
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
	TaskID       string   `json:"task_id,omitempty"`
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
		printPerRunBudgetStatus(out, reports)
		printPeriodBudgetStatus(out, reports)
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
	printPerRunBudgetStatus(out, reports)
	printPeriodBudgetStatus(out, reports)
	printGatePushStatus(out, reports)
}

// printPerRunBudgetStatus 打印 per_run 成本上限的设防情况（#40）。
//
// **没有任何拒绝记录时也要打印**：6.8 第 3 条要的是「不允许静默」。一个没设上限的系统
// 如果什么都不显示，和一个设了很大上限的系统长得一模一样——看不出区别就等于没有区别。
func printPerRunBudgetStatus(out io.Writer, reports []gateRefusalsReport) {
	var found bool
	for _, r := range reports {
		if r.PerRunBudget != nil {
			found = true
			break
		}
	}
	fmt.Fprintf(out, "\n单轮成本上限（#40）\n")
	if !found {
		fmt.Fprintf(out, "两个进程都没有报告这一层——这道闸门只在 API 进程里，确认服务端已带着本次改动重启。\n")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "来源\t状态")
	for _, r := range reports {
		if r.PerRunBudget == nil {
			continue
		}
		switch {
		case r.PerRunBudget.ParseError != "":
			fmt.Fprintf(tw, "%s\t配错了：%s（按未配置处理）\n", r.Source, r.PerRunBudget.ParseError)
		case !r.PerRunBudget.Configured:
			fmt.Fprintf(tw, "%s\t未设上限\n", r.Source)
		default:
			fmt.Fprintf(tw, "%s\t$%.4f\n", r.Source, r.PerRunBudget.BudgetUSD)
		}
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "gate-refusals: 渲染单轮成本上限失败: %v\n", err)
	}

	// 收尾这句要说**当前这个状态**意味着什么，不能一律说「没配上」。人明明配了、
	// 只是配错时，再让他去配一次是在骗他。
	var anyParseError, anyConfigured bool
	for _, r := range reports {
		if r.PerRunBudget == nil {
			continue
		}
		if r.PerRunBudget.ParseError != "" {
			anyParseError = true
		} else if r.PerRunBudget.Configured {
			anyConfigured = true
		}
	}
	switch {
	case anyParseError:
		fmt.Fprintf(out, "读不懂的值按**未配置**处理，不熔断。它不会被当成 0——那等于「什么都熔断」。\n")
	case anyConfigured:
		fmt.Fprintf(out, "已设防：一轮 Run 的折算成本越过上限就停掉它，每轮单独计。\n")
	default:
		fmt.Fprintf(out, "未设上限时**不熔断**，这是明示的而不是漏的；配上 %s 才生效。\n",
			service.RunCostBudgetEnvVar)
	}
}

// printPeriodBudgetStatus 打印四个周期维度的设防情况（#41）。
//
// 与 per_run 那块同样的道理：**没有任何拒绝记录时也要打印**。而且这一层比 per_run
// 更需要它——四个维度各自独立，人很容易只配了其中一个就以为全都设防了。
//
// **窗口起止要打出来**，不能只打一个上限数字：只看见「$10/月」的人还是不知道「这个
// 月」从哪一刻算起、在哪个时区切。周期边界是要给人看的，不是内部实现细节。
func printPeriodBudgetStatus(out io.Writer, reports []gateRefusalsReport) {
	var found *gateRefusalsPeriodBudget
	for i := range reports {
		if reports[i].PeriodBudget != nil {
			found = reports[i].PeriodBudget
			break
		}
	}
	fmt.Fprintf(out, "\n周期成本上限（#41，per_agent / per_workspace）\n")
	if found == nil {
		fmt.Fprintf(out, "两个进程都没有报告这一层——这道闸门跑在派单收口上，确认服务端已带着本次改动重启。\n")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "维度\t状态\t本周期从\t到")
	for _, st := range found.Statuses {
		state := "未设上限"
		switch {
		case st.ParseError != "":
			state = "配错了（按未配置处理）"
		case st.Configured:
			state = fmt.Sprintf("$%.4f", st.BudgetUSD)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			periodDimensionLabel(st.Key), state,
			shortRFC3339(st.Window.Start), shortRFC3339(st.Window.End))
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "gate-refusals: 渲染周期成本上限失败: %v\n", err)
	}
	// 收尾那句要分三种情况说，不能一律说「没设上限」。
	//
	// 「配错了」和「没设」在表格里是两行不同的字，但如果收尾一律说「四个维度都没设
	// 上限」，配错的人读到的是「我没设，所以不熔断」——可他明明设了，只是没生效。这
	// 比不显示更糟：他以为自己知道系统为什么没拦。
	anyConfigured, anyBroken := false, false
	for _, st := range found.Statuses {
		if st.Configured {
			anyConfigured = true
		}
		if st.ParseError != "" {
			anyBroken = true
		}
	}
	switch {
	case anyBroken && anyConfigured:
		fmt.Fprintf(out, "有维度配错了（见上表），已按未配置处理；其余维度按上表生效。\n")
	case anyBroken:
		fmt.Fprintf(out, "有维度配错了（见上表），已按未配置处理——**不是**「没设上限」，是你设的值读不懂，所以没生效。\n")
	case anyConfigured:
		fmt.Fprintf(out, "按 UTC 切日、按自然月切月；周期一到自动归零，不需要人放行。\n")
	default:
		fmt.Fprintf(out, "四个维度都没设上限，不熔断。按 UTC 切日、按自然月切月，周期到了自己归零。\n")
	}
}

// shortRFC3339 把 ISO 时间戳压成「月-日 时:分」，够看清周期边界又不占宽度。
func shortRFC3339(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.UTC().Format("01-02 15:04")
}

// periodDimensionLabel 把 `agent_daily` 翻成中文，与表格里其他列的中文保持一致。
func periodDimensionLabel(key string) string {
	switch key {
	case "agent_daily":
		return "单 Agent · 日"
	case "agent_monthly":
		return "单 Agent · 月"
	case "workspace_daily":
		return "整个工作区 · 日"
	case "workspace_monthly":
		return "整个工作区 · 月"
	default:
		return key
	}
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
	case "run_cost_budget_exceeded":
		return "单轮成本"
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
		detail := fmt.Sprintf("已花 $%.4f / 上限 $%.4f；%d 行计价、%d 行算不出来",
			*n.SpentUSD, *n.BudgetUSD, derefInt(n.PricedRows), derefInt(n.UnpricedRows))
		// per_run 是按 Run 判的，指到那一轮——否则「这条 Issue 上有一轮超了」等于
		// 没说，重跑时不知道该看哪个。
		if n.TaskID != "" {
			detail = fmt.Sprintf("Run %s：%s", shortUUID(n.TaskID), detail)
		}
		return detail
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
