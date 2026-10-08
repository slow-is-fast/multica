package handler

// Ruel 新增：把「用量行的模型名采没采到」变成一个能主动查的指标（#34）。
//
// 为什么要有这个端点：#32 修掉的那条漏采连续污染了本机每一条 codex 用量，期间没有任何
// 信号指向它，最后是有人在算账单时才撞见。**下一次犯同类错误，未必有人正好在算账单。**
// 本端点就是那双眼睛——不用等谁去算账，问一句就有。
//
// 刻意做成进程内累计（见 internal/metrics/model_completeness.go）：它回答的是「这台服务
// 此刻有没有在漏」。要问「历史上漏了多少」，扫 `task_usage.model` 更准，那是另一个问题。
//
// 放在用户级（只要求登录，不校验 workspace）的理由：它不含任何 workspace 的资源，是整台
// 服务的运行状况。挂在某个 workspace 下反而会让人以为「这个数是我这个空间的」。

import (
	"net/http"

	obsmetrics "github.com/multica-ai/multica/server/internal/metrics"
)

// GetUsageCompleteness 返回按 provider 分组的模型名采集完整度。
func (h *Handler) GetUsageCompleteness(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, obsmetrics.DefaultModelCompleteness.Snapshot())
}
