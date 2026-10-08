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
)

// GetGateRefusals 返回本进程记录的闸门拒绝回执，最新的在前。
func (h *Handler) GetGateRefusals(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"generated_at": time.Now().UTC(),
		"process":      "api",
		"notices":      gaterefusal.Snapshot(),
	})
}
