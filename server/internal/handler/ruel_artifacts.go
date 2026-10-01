package handler

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/ruel/artifacts"
)

// Ruel 新增：Run 产物的写入与读取。
//
// 上游把 Run 结果放在 agent_task_queue.result 这个 jsonb 里，只有 output / pr_url /
// work_dir / session_id 四项。P0-6 要求「Run 链接到隔离分支、diff 与测试证据」，缺了
// 产物落点验收就没有落脚点，所以这里补一组最小的读写接口。
//
// 刻意放在独立文件而不是并进 daemon.go：将来同步上游时，这个文件不会与上游改动打架。

// RuelArtifactRequest 是 daemon 上报产物的请求体。
//
// 只收 kind 与 content，不收 checksum——校验值由服务端用同一份内容重算。收下客户端
// 给的 checksum 等于把「可核对」交回给被校验的一方，那样校验就没有意义了。
type RuelArtifactRequest struct {
	Kind    string `json:"kind"`
	Content string `json:"content"`
	URI     string `json:"uri"`
}

// RuelArtifactResponse 是读取产物的响应体。content 可能很长（diff 全文），由调用方
// 决定是否截断；checksum 始终返回，便于核对截断后的内容是否被改过。
type RuelArtifactResponse struct {
	ID        string `json:"id"`
	TaskID    string `json:"task_id"`
	Kind      string `json:"kind"`
	URI       string `json:"uri"`
	Size      int64  `json:"size"`
	Checksum  string `json:"checksum"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}

func toArtifactResponses(in []artifacts.Artifact) []RuelArtifactResponse {
	out := make([]RuelArtifactResponse, 0, len(in))
	for _, a := range in {
		out = append(out, RuelArtifactResponse{
			ID:        uuidToString(a.ID),
			TaskID:    uuidToString(a.TaskID),
			Kind:      a.Kind,
			URI:       a.URI,
			Size:      a.Size,
			Checksum:  a.Checksum,
			Content:   a.Content,
			CreatedAt: a.CreatedAt.Time.String(),
		})
	}
	return out
}

// UpsertRuelArtifact 由 daemon 在 Run 结束后调用，写入该 Run 的一条产物。
// 同一 Run 的同类产物覆盖写入，保留最新一份。
func (h *Handler) UpsertRuelArtifact(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")

	// 复用上游的 daemon 任务鉴权：调用方必须持有该任务的 workspace。
	task, workspaceID, ok := h.requireDaemonTaskAccessWithWorkspace(w, r, taskID)
	if !ok {
		return
	}

	var req RuelArtifactRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	kind := strings.TrimSpace(req.Kind)
	if kind == "" {
		writeError(w, http.StatusBadRequest, "kind is required")
		return
	}
	if req.Content == "" {
		// 空产物不是「没有改动」，就是没采集到。这两者必须分开：前者是合法结果，
		// 后者是缺陷。静默写入空产物，会让 Issue 页上的「变更：无」变得无法判断。
		writeError(w, http.StatusBadRequest, "content is required; report absence explicitly instead")
		return
	}

	store := artifacts.NewStore(h.DB)
	err := store.Upsert(r.Context(), artifacts.Artifact{
		WorkspaceID: parseUUID(workspaceID),
		TaskID:      parseUUID(taskID),
		IssueID:     task.IssueID,
		Kind:        kind,
		URI:         req.URI,
		Checksum:    artifacts.Sum(req.Content),
		Content:     req.Content,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ListRuelTaskArtifacts 列出某次 Run 的产物（daemon 侧读取）。
func (h *Handler) ListRuelTaskArtifacts(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	if _, _, ok := h.requireDaemonTaskAccessWithWorkspace(w, r, taskID); !ok {
		return
	}
	list, err := artifacts.NewStore(h.DB).ListByTask(r.Context(), parseUUID(taskID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list artifacts")
		return
	}
	writeJSON(w, http.StatusOK, toArtifactResponses(list))
}

// ListRuelIssueArtifacts 列出某个 Issue 下所有 Run 的产物（人侧读取）。
//
// 用 Issue 而不是 Run 作为维度，是因为验收看的是「这个需求总共改了什么」。多轮 Run
// 的变更要能一起看到——只看最后一轮会漏掉被 revert 又重做的改动。
func (h *Handler) ListRuelIssueArtifacts(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	issue, ok := h.loadIssueForUser(w, r, id)
	if !ok {
		return
	}
	list, err := artifacts.NewStore(h.DB).ListByIssue(r.Context(), pgtype.UUID(issue.ID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list artifacts")
		return
	}
	writeJSON(w, http.StatusOK, toArtifactResponses(list))
}
