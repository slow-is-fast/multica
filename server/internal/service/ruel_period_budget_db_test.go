package service

// #41 的**落库**那一半。service 包里那一组（ruel_period_budget_test.go）证明的是
// 「边界算得对、钱算得对」，这里证明的是**闸门真的挡住了派单**——中间隔着一次真实的
// SQL 聚合，而恰恰是这一层最容易出错：作用域走的是 task -> agent / task -> issue ->
// workspace 两条 JOIN，窗口边界在 SQL 里有没有真的闭合，脱离数据库一个字都测不出来。
//
// 三个必须钉住的点：
//  1. 窗口的**上界**在 SQL 里生效（只写 `>= since` 会把后面周期的行也算进来）
//  2. workspace 维度真的**跨 agent 求和**（否则它只是 agent 维度的别名）
//  3. 挡住的是**派单**这个动作，不是只返回一个错误

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/gaterefusal"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type ruelPeriodFixture struct {
	pool        *pgxpool.Pool
	workspaceID string
	userID      string
	agentA      string
	agentB      string
	runtimeID   string
	issueA      string
	issueB      string
	svc         *TaskService
}

// seedRuelPeriodFixture 起两个 agent、两条 Issue，都在同一个 workspace 里。
//
// 两个 agent 是必需的第二条：**workspace 维度要跨 agent 求和**，只有一个 agent 时，
// 「按 agent 求和」与「按 workspace 求和」给出同一个数，测试分辨不出实现是不是把
// 两个维度写成了同一个。
func seedRuelPeriodFixture(t *testing.T) *ruelPeriodFixture {
	t.Helper()
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	workspaceID, userID, agentA, issueA := seedAttributionFixture(t, pool)

	var runtimeID string
	if err := pool.QueryRow(ctx, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentA).Scan(&runtimeID); err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	var agentB string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent (workspace_id, name, runtime_mode, runtime_config, runtime_id, visibility,
			max_concurrent_tasks, owner_id, instructions, custom_env, custom_args)
		VALUES ($1, 'ruel-period-agent-b', 'cloud', '{}'::jsonb, $2, 'workspace', 1, $3, '', '{}'::jsonb, '[]'::jsonb)
		RETURNING id`, workspaceID, runtimeID, userID).Scan(&agentB); err != nil {
		t.Fatalf("seed second agent: %v", err)
	}
	var issueB string
	if err := pool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, title, creator_type, creator_id, assignee_type, assignee_id, priority, number)
		VALUES ($1, 'ruel period issue b', 'member', $2, 'agent', $3, 'medium', 2)
		RETURNING id`, workspaceID, userID, agentB).Scan(&issueB); err != nil {
		t.Fatalf("seed second issue: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE issue SET status = 'in_progress' WHERE id IN ($1, $2)`, issueA, issueB); err != nil {
		t.Fatalf("activate issues: %v", err)
	}

	return &ruelPeriodFixture{
		pool:        pool,
		workspaceID: workspaceID,
		userID:      userID,
		agentA:      agentA,
		agentB:      agentB,
		runtimeID:   runtimeID,
		issueA:      issueA,
		issueB:      issueB,
		svc:         &TaskService{Queries: db.New(pool)},
	}
}

// runFor 给一个 agent 在一条 Issue 上起一轮 Run（终态，只为挂用量用）。
func (f *ruelPeriodFixture) runFor(t *testing.T, agentID, issueID string) string {
	t.Helper()
	var taskID string
	if err := f.pool.QueryRow(context.Background(), `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority,
			originator_user_id, accountable_user_id, originator_source)
		VALUES ($1, $2, $3, 'completed', 0, $4, $4, 'direct_human')
		RETURNING id`, agentID, f.runtimeID, issueID, f.userID).Scan(&taskID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	t.Cleanup(func() {
		f.pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID)
	})
	return taskID
}

// usage 挂一条用量行。createdAt 显式给，因为窗口边界只能靠它来测。
//
// provider 用 seq 区分：task_usage 上有 (task_id, provider, model) 唯一约束，同一轮
// Run 上挂多条 model 相同的行必须换 provider。
func (f *ruelPeriodFixture) usage(t *testing.T, taskID, model string, in int64, createdAt time.Time, seq int) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO task_usage (task_id, provider, model, input_tokens, created_at)
		VALUES ($1, $2, $3, $4, $5)`, taskID, fmt.Sprintf("p%d", seq), model, in, createdAt); err != nil {
		t.Fatalf("seed usage: %v", err)
	}
}

// TestPeriodBudgetGateBlocksTheDispatchThatWouldGoOver 是 #41 的主验收用例。
//
// 走到 EnqueueTaskForIssue 而不是直接调闸门函数：判据要的是「任一触限即停」，而
// 「停」的意义在于**那一单没有被派出去**。只调闸门函数能证明它返回了错误，证明不了
// 派单路径真的听它的话——中间还隔着深度闸门、委派预算闸门两条也会拒绝的路径。
func TestPeriodBudgetGateBlocksTheDispatchThatWouldGoOver(t *testing.T) {
	gaterefusal.Reset()
	defer gaterefusal.Reset()
	f := seedRuelPeriodFixture(t)
	// 上限 $0.50；下面那一条用量实测 $1.00（glm-5 input $1.00/百万）。
	t.Setenv(EnvAgentDailyBudgetUSD, "0.50")
	t.Setenv(EnvAgentMonthlyBudgetUSD, "")
	t.Setenv(EnvWorkspaceDailyBudgetUSD, "")
	t.Setenv(EnvWorkspaceMonthlyBudgetUSD, "")

	taskID := f.runFor(t, f.agentA, f.issueA)
	f.usage(t, taskID, "glm-5", 1_000_000, time.Now().UTC(), 1)

	_, err := f.svc.EnqueueTaskForIssue(context.Background(), db.Issue{
		ID:           util.MustParseUUID(f.issueA),
		WorkspaceID:  util.MustParseUUID(f.workspaceID),
		AssigneeID:   util.MustParseUUID(f.agentA),
		Priority:     "medium",
		Status:       "in_progress",
		CreatorID:    util.MustParseUUID(f.userID),
		CreatorType:  "member",
		AssigneeType: pgtype.Text{String: "agent", Valid: true},
	})
	if !errors.Is(err, ErrPeriodBudgetExceeded) {
		t.Fatalf("已花 $1.00、上限 $0.50，派单却没被挡住（err=%v）", err)
	}

	// 库里必须**没有**多出那一单：闸门返回错误但行已经插进去了，等于没挡住。
	var queued int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status = 'queued'`, f.issueA).Scan(&queued); err != nil {
		t.Fatalf("count queued: %v", err)
	}
	if queued != 0 {
		t.Fatalf("派单被挡了，库里却多了 %d 条 queued——错误返回了但动作发生了，等于没挡", queued)
	}

	// 回执要能指出是哪个维度：四个维度修法不同，混在一起人不知道该调哪一个。
	var found *gaterefusal.Notice
	snap := gaterefusal.Snapshot()
	for i := range snap {
		if snap[i].Dimension == gaterefusal.DimensionPeriodBudget+".agent_daily" {
			found = &snap[i]
			break
		}
	}
	if found == nil {
		t.Fatal("挡住了却没留下回执——这正是 #33 要修的那种静默")
	}
	if found.SpentUSD == nil || found.BudgetUSD == nil || *found.SpentUSD < *found.BudgetUSD {
		t.Fatalf("回执的已花/上限自相矛盾：%v / %v", found.SpentUSD, found.BudgetUSD)
	}
	t.Logf("已挡：agent_daily 已花 $%.4f / 上限 $%.4f", *found.SpentUSD, *found.BudgetUSD)
}

// TestPeriodBudgetWorkspaceScopeAddsAcrossAgents 钉住 workspace 维度真的跨 agent 求和。
//
// 每个 agent 各花 $1.00，各自都没超 $1.50，但 workspace 合计 $2.00 超了。只有一个
// agent 时这条测试是绿的（对照组），两个 agent 才红——所以它分辨得出「按 workspace
// 求和」与「按 agent 求和」是不是被写成了同一件事。
func TestPeriodBudgetWorkspaceScopeAddsAcrossAgents(t *testing.T) {
	gaterefusal.Reset()
	defer gaterefusal.Reset()
	f := seedRuelPeriodFixture(t)
	t.Setenv(EnvAgentDailyBudgetUSD, "")
	t.Setenv(EnvAgentMonthlyBudgetUSD, "")
	t.Setenv(EnvWorkspaceDailyBudgetUSD, "1.50")
	t.Setenv(EnvWorkspaceMonthlyBudgetUSD, "")

	now := time.Now().UTC()
	runA := f.runFor(t, f.agentA, f.issueA)
	f.usage(t, runA, "glm-5", 1_000_000, now, 1)

	agentID := util.MustParseUUID(f.agentA)
	wsID := util.MustParseUUID(f.workspaceID)
	path := "workspace_scope_test"

	// 对照组：只有一个 agent 花 $1.00 < $1.50，放行。
	if err := f.svc.ruelGuardPeriodBudget(context.Background(), agentID, wsID, util.MustParseUUID(f.issueA), path); err != nil {
		t.Fatalf("workspace 合计 $1.00 < 上限 $1.50 却挡了：%v", err)
	}

	// 第二个 agent 再花 $1.00 → 合计 $2.00 > $1.50。
	runB := f.runFor(t, f.agentB, f.issueB)
	f.usage(t, runB, "glm-5", 1_000_000, now, 1)

	err := f.svc.ruelGuardPeriodBudget(context.Background(), agentID, wsID, util.MustParseUUID(f.issueA), path)
	if !errors.Is(err, ErrPeriodBudgetExceeded) {
		t.Fatalf("两个 agent 合计 $2.00 > 上限 $1.50，却没挡住（err=%v）——workspace 维度若退化成 agent 维度，这里会是绿的", err)
	}
	if !strings.Contains(err.Error(), "workspace_daily") {
		t.Fatalf("错误信息没指出是 workspace_daily：%q。四个维度修法不同，指不出等于没指", err.Error())
	}
}

// TestPeriodBudgetReceiptsStaySeparatePerAgent 钉住回执按 agent 分开。
//
// 为什么值得单开：合并键若不含 agent，同一条 Issue 上先后被挡的两个 agent 会揉成一条
// 回执，而 agent_id 是「保留第一次」的字段、不刷新，于是后撞墙那个 agent 在回执里
// **彻底消失**——「哪个 agent 撞了墙」正是这条回执要回答的问题。
//
// **必须用同一条 Issue**：IssueID 本身就在合并键里，两个 agent 各挂各的 Issue 时，
// 不管合并键有没有 agent 都会分成两条，测试就失去了区分力（突变校验里「摘掉 agent」
// 那条一度没让它变红，就是栽在这上面）。同一条 Issue 换 agent 是真实场景：Issue 改派
// 之后，新旧两个 agent 可能在同一段时间内先后撞上同一个上限。
func TestPeriodBudgetReceiptsStaySeparatePerAgent(t *testing.T) {
	gaterefusal.Reset()
	defer gaterefusal.Reset()
	f := seedRuelPeriodFixture(t)
	// 两个 agent 各自花 $1.00，上限 $0.50，各自都超。
	t.Setenv(EnvAgentDailyBudgetUSD, "0.50")
	t.Setenv(EnvAgentMonthlyBudgetUSD, "")
	t.Setenv(EnvWorkspaceDailyBudgetUSD, "")
	t.Setenv(EnvWorkspaceMonthlyBudgetUSD, "")

	now := time.Now().UTC()
	f.usage(t, f.runFor(t, f.agentA, f.issueA), "glm-5", 1_000_000, now, 1)
	f.usage(t, f.runFor(t, f.agentB, f.issueA), "glm-5", 1_000_000, now, 1)

	ctx := context.Background()
	wsID := util.MustParseUUID(f.workspaceID)
	issueID := util.MustParseUUID(f.issueA) // 同一条 Issue，只换 agent
	dim := gaterefusal.DimensionPeriodBudget + ".agent_daily"
	for _, agent := range []string{f.agentA, f.agentB} {
		err := f.svc.ruelGuardPeriodBudget(ctx, util.MustParseUUID(agent), wsID, issueID, "receipt_test")
		if !errors.Is(err, ErrPeriodBudgetExceeded) {
			t.Fatalf("agent %s 超限却没被挡：%v", agent, err)
		}
	}

	seen := map[string]bool{}
	for _, n := range gaterefusal.Snapshot() {
		if n.Dimension != dim {
			continue
		}
		if n.IssueID == "" {
			t.Fatalf("回执没有 issue_id（agent %s）——人知道这个 agent 超了，却不知道哪条 Issue 上那一单没派出去", n.AgentID)
		}
		if seen[n.AgentID] {
			t.Fatalf("agent %s 的回执被合并掉了", n.AgentID)
		}
		seen[n.AgentID] = true
	}
	if len(seen) != 2 {
		t.Fatalf("按 agent 分开的回执数 = %d, want 2（%v）", len(seen), seen)
	}
}

// TestPeriodBudgetWindowIsEnforcedOnBothSides 钉住窗口的**上界**真的进了 SQL。
//
// 为什么值得单开：只写 `created_at >= since` 在「查当前周期」这个唯一的使用场景下看
// 不出错（未来的行还不存在），但它让「每一行恰好属于一个周期」这句话在库里不成立。
// 一旦以后有人拿它去查历史窗口，后面所有周期的行都会漏进来。上界必须现在就在。
func TestPeriodBudgetWindowIsEnforcedOnBothSides(t *testing.T) {
	f := seedRuelPeriodFixture(t)
	now := time.Now().UTC()
	runA := f.runFor(t, f.agentA, f.issueA)
	f.usage(t, runA, "glm-5", 1_000_000, now, 1)

	ctx := context.Background()
	agentID := util.MustParseUUID(f.agentA)
	wsID := util.MustParseUUID(f.workspaceID)
	dim := PeriodBudgetDimension{ScopeAgent, PeriodDay}

	// 今天的窗口：应当算到这一行。
	today, err := f.svc.ruelPeriodCostFor(ctx, dim, agentID, wsID, periodWindow(now, PeriodDay))
	if err != nil {
		t.Fatalf("query today window: %v", err)
	}
	if today.PricedRows != 1 {
		t.Fatalf("今天的窗口算到 %d 行, want 1", today.PricedRows)
	}

	// 昨天的窗口 [昨天 00:00, 今天 00:00)：这一行是今天写的，不该算进去。
	// 只有下界的 SQL 在这里会把它也算上——上界缺失就会在此变红。
	yesterday := periodWindow(now.Add(-24*time.Hour), PeriodDay)
	past, err := f.svc.ruelPeriodCostFor(ctx, dim, agentID, wsID, yesterday)
	if err != nil {
		t.Fatalf("query yesterday window: %v", err)
	}
	if past.PricedRows != 0 || past.UnpricedRows != 0 {
		t.Fatalf("昨天的窗口算到 priced %d / unpriced %d 行, want 0 / 0——上界没进 SQL", past.PricedRows, past.UnpricedRows)
	}
	if !yesterday.End.Equal(periodWindow(now, PeriodDay).Start) {
		t.Fatalf("昨天窗口的终点 %s 不等于今天窗口的起点 %s——周期之间有洞或重叠", yesterday.End, periodWindow(now, PeriodDay).Start)
	}
}

// TestPeriodBudgetGateCannotJudgeWhatItCannotPrice：与 #40 同源的四态口径推论。
// 一条可计价的行都没有时，放行——但那是「不知道花了多少」，不是「没花钱」。
func TestPeriodBudgetGateCannotJudgeWhatItCannotPrice(t *testing.T) {
	gaterefusal.Reset()
	defer gaterefusal.Reset()
	f := seedRuelPeriodFixture(t)
	// 上限取 **0**（「一分钱都不许花」）而不是一个很小的正数：小正数下「累计 0 < 上限」
	// 本身就足以放行，会掩盖掉「零条可计价即放行」这条分支——把它换成 0，只有这条分支
	// 能放行（0 < 0 不成立），所以摘掉这条分支测试立刻红。
	t.Setenv(EnvAgentDailyBudgetUSD, "0")
	t.Setenv(EnvAgentMonthlyBudgetUSD, "")
	t.Setenv(EnvWorkspaceDailyBudgetUSD, "")
	t.Setenv(EnvWorkspaceMonthlyBudgetUSD, "")

	taskID := f.runFor(t, f.agentA, f.issueA)
	f.usage(t, taskID, "unknown-model", 999_999_999, time.Now().UTC(), 1)

	err := f.svc.ruelGuardPeriodBudget(context.Background(),
		util.MustParseUUID(f.agentA), util.MustParseUUID(f.workspaceID), util.MustParseUUID(f.issueA), "unpriced_test")
	if err != nil {
		t.Fatalf("算不出价格的用量被当成超限了：%v。无法判定 ≠ 超限，这是 #32 踩过的坑", err)
	}
	for _, n := range gaterefusal.Snapshot() {
		if n.Dimension == gaterefusal.DimensionPeriodBudget+".agent_daily" {
			t.Fatal("放行时却记了触限回执——回执是「我挡了什么」的证据，不是「我看了什么」的日志")
		}
	}
}

// TestPeriodBudgetGateUnconfiguredStaysOutOfTheWay：不配置就不熔断。
// 判据要的是「未配置时不熔断，界面明示未设上限」——所以它既不能按某个默认值偷偷
// 截断，也不能一声不吭地挡住。
func TestPeriodBudgetGateUnconfiguredStaysOutOfTheWay(t *testing.T) {
	gaterefusal.Reset()
	defer gaterefusal.Reset()
	f := seedRuelPeriodFixture(t)
	t.Setenv(EnvAgentDailyBudgetUSD, "")
	t.Setenv(EnvAgentMonthlyBudgetUSD, "")
	t.Setenv(EnvWorkspaceDailyBudgetUSD, "")
	t.Setenv(EnvWorkspaceMonthlyBudgetUSD, "")

	taskID := f.runFor(t, f.agentA, f.issueA)
	// 一亿 token 的 glm-5 ≈ $100，够贵了。
	f.usage(t, taskID, "glm-5", 100_000_000, time.Now().UTC(), 1)

	if err := f.svc.ruelGuardPeriodBudget(context.Background(),
		util.MustParseUUID(f.agentA), util.MustParseUUID(f.workspaceID), util.MustParseUUID(f.issueA), "unconfigured_test"); err != nil {
		t.Fatalf("四个维度都没设却挡了派单：%v", err)
	}
}
