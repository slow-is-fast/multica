package service

// #41 的测试。分两组：
//
//  1. **周期边界**（判据点名要求写清楚的）：时区、跨月、半开区间。这三条不是实现细节，
//     是「今天算到几点」这类扯皮的裁决依据，必须有测试钉住，否则下次有人改成本地时区
//     或滚动 30 天窗口，没人会发现。
//  2. **判定语义**：未配置不设防、读不懂按未配置、零条可计价时放行、unpriced 跳过但计数。

import (
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestPeriodStartUsesUTCDayBoundary(t *testing.T) {
	// 时刻必须**跨日**：2026-03-01 07:00 +08:00 的 UTC 是 2026-02-28 23:00，两地是不
	// 同一天。若这里挑一个两地同日的时刻（比如 12:00 +08:00），按本地时区切日也会算
	// 出同一个日期，测试就失去了区分力——突变校验里「改用本地时区」那条一度没让它变
	// 红，就是栽在这上面。
	beijing := time.FixedZone("CST", 8*60*60)
	at := time.Date(2026, 3, 1, 7, 0, 0, 0, beijing)
	if at.UTC().Day() == at.Day() {
		t.Fatalf("测试样本选错了：%s 在两地是同一天，测不出时区差异", at)
	}
	got := PeriodStart(at, PeriodDay)
	want := time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("PeriodStart = %s, want %s（必须按 UTC 切日，否则同一时刻在不同时区的实例上会落进不同的周期）", got, want)
	}
	// 反证：按本地时区切日会给出 2026-03-01 00:00 +08:00，与 UTC 答案不是同一瞬间。
	local := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, beijing)
	if local.Equal(want) {
		t.Fatal("本地时区与 UTC 给出了同一个瞬间，样本无法区分两种实现")
	}
}

func TestPeriodStartUsesNaturalMonthNotRollingThirtyDays(t *testing.T) {
	// 3 月 31 日：自然月的起点是 3 月 1 日，不是「30 天前」（那会是 3 月 1 日以外的某天）。
	// 滚动窗口的终点一直在动，人没法回答「这个月花了多少」。
	at := time.Date(2026, 3, 31, 23, 59, 0, 0, time.UTC)
	got := PeriodStart(at, PeriodMonth)
	want := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("PeriodStart(month) = %s, want %s", got, want)
	}
}

func TestPeriodEndIsNextNaturalMonthAndCoversShortMonths(t *testing.T) {
	// AddDate 在月末会归一化（1/31 + 1 月 = 3/3），所以必须用「下个月 1 日」而不是
	// 「加一个月」。2 月是最容易露馅的那个：28 天的月，终点得是 3 月 1 日。
	cases := []struct {
		at   time.Time
		want time.Time
	}{{time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 12, 5, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		if got := PeriodEnd(c.at, PeriodMonth); !got.Equal(c.want) {
			t.Fatalf("PeriodEnd(%s, month) = %s, want %s（跨年也要对）", c.at, got, c.want)
		}
	}
}

// TestPeriodWindowsTileWithoutGapOrOverlap 钉住「每一行恰好属于一个周期」。
//
// 半开区间 `[start, end)` 的全部意义就在这里：闭区间会让落在边界瞬间的行算两次，开
// 区间会让它两边都漏。连续两个周期必须首尾相接，差一纳秒都是洞。
func TestPeriodWindowsTileWithoutGapOrOverlap(t *testing.T) {
	for _, kind := range []PeriodKind{PeriodDay, PeriodMonth} {
		at := time.Date(2026, 3, 15, 10, 30, 0, 0, time.UTC)
		w1 := periodWindow(at, kind)
		if !w1.Start.Before(w1.End) {
			t.Fatalf("%s: start %s 不在 end %s 之前", kind, w1.Start, w1.End)
		}
		w2 := periodWindow(w1.End, kind)
		if !w2.Start.Equal(w1.End) {
			t.Fatalf("%s: 下一个周期起点 %s 不等于上一个终点 %s——中间有洞或重叠", kind, w2.Start, w1.End)
		}
		// 边界那一瞬属于**后**一个周期。
		if !PeriodStart(w1.End, kind).Equal(w1.End) {
			t.Fatalf("%s: 边界瞬间 %s 没被算进下一个周期", kind, w1.End)
		}
	}
}

func TestPeriodBudgetUnsetMeansNoCeiling(t *testing.T) {
	for _, d := range AllPeriodBudgetDimensions {
		t.Setenv(d.EnvVar(), "")
	}
	st := PeriodBudgetStatusFromEnv(time.Now())
	if st.AnyConfigured() {
		t.Fatal("四个维度都没设却报了已设防")
	}
	for _, s := range st.Statuses {
		if s.Configured || s.ParseError != "" {
			t.Fatalf("%s: configured=%v parse_error=%q，want 未配置且无报错", s.Dimension.Key(), s.Configured, s.ParseError)
		}
	}
}

// TestPeriodBudgetRejectsGarbageLoudly：读不懂的值要报出来，且**不退化成 0**——
// 退化成 0 等于「什么都熔断」。
func TestPeriodBudgetRejectsGarbageLoudly(t *testing.T) {
	t.Setenv(EnvAgentDailyBudgetUSD, "ten dollars")
	t.Setenv(EnvWorkspaceMonthlyBudgetUSD, "-3")
	st := PeriodBudgetStatusFromEnv(time.Now())
	byKey := map[string]PeriodBudgetConfig{}
	for _, s := range st.Statuses {
		byKey[s.Dimension.Key()] = s
	}
	if byKey["agent_daily"].ParseError == "" {
		t.Fatal("agent_daily 配了非数字却没报错")
	}
	if byKey["agent_daily"].Configured {
		t.Fatal("读不懂的值不能算已配置")
	}
	if byKey["workspace_monthly"].ParseError == "" {
		t.Fatal("负数上限必须报错——负上限在比较时会永久触限，等于「什么都熔断」")
	}
	if st.AnyConfigured() {
		t.Fatal("配错的值不该让 AnyConfigured 为真")
	}
}

func TestPeriodBudgetConfiguredDimensionsReportTheirWindow(t *testing.T) {
	t.Setenv(EnvAgentDailyBudgetUSD, "1.5")
	now := time.Date(2026, 3, 15, 10, 30, 0, 0, time.UTC)
	st := PeriodBudgetStatusFromEnv(now)
	for _, s := range st.Statuses {
		if s.Dimension.Key() != "agent_daily" {
			continue
		}
		if !s.Configured || s.USD != 1.5 {
			t.Fatalf("agent_daily = %v / %v, want true / 1.5", s.Configured, s.USD)
		}
		// 窗口也要带出来：只给一个上限数字，人还是不知道「今天」算到几点。
		if !s.Window.Start.Equal(time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)) {
			t.Fatalf("window start = %s, want 2026-03-15 00:00 UTC", s.Window.Start)
		}
	}
}

func periodUsageRow(model string, in int64) db.TaskUsage {
	return db.TaskUsage{
		Model:       model,
		InputTokens: in,
		CreatedAt:   pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
}

// TestPeriodUsageCostSkipsUnpricedButCountsThem：与 #28 / #40 同一条口径。
// unpriced 按 0 计入会让累计永远追不上上限，而纯 unpriced 的链会让这一层**永不触发**
// （#32 的连带效应）——所以条数必须跟着判定一起暴露。
func TestPeriodUsageCostSkipsUnpricedButCountsThem(t *testing.T) {
	acc := ruelPeriodUsageCost([]db.TaskUsage{
		periodUsageRow("glm-5", 1_000_000),
		periodUsageRow("unknown", 999_999_999),
	})
	if acc.PricedRows != 1 || acc.UnpricedRows != 1 {
		t.Fatalf("rows = priced %d / unpriced %d, want 1 / 1", acc.PricedRows, acc.UnpricedRows)
	}
	if acc.PricedUSD < 0.9 || acc.PricedUSD > 1.1 {
		t.Fatalf("PricedUSD = %.10f, want 约 1.0（unpriced 的十亿 token 不得贡献任何金额）", acc.PricedUSD)
	}
}

// TestPeriodBudgetDimensionKeysAndEnvVars 钉住四个维度的名字各自独立。
// 合并成「一个综合分」之后没人说得清是因为什么停的，也就没法决定调哪个上限。
// TestPeriodBudgetStatusJSONUsesSnakeCaseKeys 钉住 API 的 JSON 形状。
//
// 为什么值得单开：Go 的 json 解码忽略大小写但**不忽略下划线**，所以消费方写
// `parse_error` 而这里输出 `ParseError` 时，那个字段会静默变成零值——界面上「配错了」
// 显示成「未设上限」，而整个过程没有任何报错。真机验证时正是这么栽的。
//
// 因此这条测的是**键名本身**，不是值：值对了键名错了，一样是坏的。
func TestPeriodBudgetStatusJSONUsesSnakeCaseKeys(t *testing.T) {
	t.Setenv(EnvAgentDailyBudgetUSD, "ten dollars")
	t.Setenv(EnvWorkspaceMonthlyBudgetUSD, "-3")
	raw, err := json.Marshal(PeriodBudgetStatusFromEnv(time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	statuses, ok := decoded["statuses"].([]any)
	if !ok || len(statuses) != 4 {
		t.Fatalf("statuses = %v, want 4 条", decoded["statuses"])
	}
	first, _ := statuses[0].(map[string]any)
	for _, want := range []string{
		"dimension", "key", "env_var", "configured", "parse_error", "window",
	} {
		if _, ok := first[want]; !ok {
			t.Fatalf("回执里缺 %q 键（现有: %v）——消费方按 snake_case 取，取不到就是静默零值", want, keysOf(first))
		}
	}
	// 配错的值必须真的出现在 JSON 里，而不只是 Go 结构体里。
	if got, _ := first["parse_error"].(string); got == "" {
		t.Fatal("agent_daily 配了非数字，JSON 里 parse_error 却是空的")
	}
	if got, _ := first["key"].(string); got != "agent_daily" {
		t.Fatalf("key = %q, want agent_daily", got)
	}
	window, _ := first["window"].(map[string]any)
	if _, ok := window["start"]; !ok {
		t.Fatalf("window 缺 start（现有: %v）", keysOf(window))
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestPeriodBudgetDimensionKeysAndEnvVars(t *testing.T) {
	seen := map[string]string{}
	for _, d := range AllPeriodBudgetDimensions {
		if _, dup := seen[d.Key()]; dup {
			t.Fatalf("维度 key 重复: %s", d.Key())
		}
		seen[d.Key()] = d.EnvVar()
	}
	want := map[string]string{
		"agent_daily":       EnvAgentDailyBudgetUSD,
		"agent_monthly":     EnvAgentMonthlyBudgetUSD,
		"workspace_daily":   EnvWorkspaceDailyBudgetUSD,
		"workspace_monthly": EnvWorkspaceMonthlyBudgetUSD,
	}
	for k, v := range want {
		if seen[k] != v {
			t.Fatalf("%s = %q, want %q", k, seen[k], v)
		}
	}
	if len(seen) != 4 {
		t.Fatalf("维度数 = %d, want 4", len(seen))
	}
}
