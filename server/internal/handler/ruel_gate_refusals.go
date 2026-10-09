package handler

// Ruel 新增：把闸门拒绝的回执暴露成端点（#33）。
//
// 回执本身记在 `internal/gaterefusal`，它活在事务之外——所以这里能查到的，正是那些
// **事务已经回滚、什么都没留下**的拒绝。这是本端点存在的全部理由：拒绝发生过，但库里
// 一行痕迹都没有。
//
// 一个必须说清的边界（同 gate refusal 包的注释）：累计是进程内的。委派链的入口有六条，
// 评论触发的那几条走 API 进程，wakeup 触发的走 daemon 进程。本端点只回答**这个进程**看到
// 的拒绝；daemon 侧那份在 daemon 的健康端口 `/gate-refusals` 上。CLI 两边都问。

import (
	"net/http"
	"time"

	"github.com/multica-ai/multica/server/internal/gaterefusal"
	"github.com/multica-ai/multica/server/internal/service"
)

// GetGateRefusals 返回本进程记录的闸门拒绝回执，最新的在前。
func (h *Handler) GetGateRefusals(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"generated_at": time.Now().UTC(),
		"process":      "api",
		"notices":      gaterefusal.Snapshot(),
		// #36：推送通道的自检面。未配置时 Configured=false，其余字段为空。
		"push": gaterefusal.PushStatusSnapshot(),
		// #40：per_run 成本上限的设防情况。放在这里是因为**这道闸门跑在 API 进程**——
		// 用量上报是 API 端点，不像委派闸门那样两进程都有份。
		//
		// 未配置必须能被看出来：一个没设上限的系统，如果界面上什么都不显示，和一个
		// 设了很大上限的系统长得一模一样。6.8 第 3 条要的「不允许静默」包括这一条。
		"per_run_budget": service.RuelRunCostBudgetStatusFromEnv(),
		// #41：四个周期维度（agent / workspace × 日 / 月）的设防情况。
		//
		// 连窗口边界一起给出去，是因为「周期」这件事本身要能被人看见：只显示一个上限
		// 数字，人还是不知道「今天」到底算到几点、这个月从哪一刻开始。
		"period_budget": service.PeriodBudgetStatusFromEnv(time.Now()),
	})
}
