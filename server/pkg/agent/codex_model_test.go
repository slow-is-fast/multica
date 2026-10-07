package agent

// Ruel 新增：#32 —— codex adapter 不上报模型名，用量记录的 model 恒为 `unknown`。
//
// 根因不是「拿不到」，是**读取时机挂错了地方**：模型名只在会话 rollout 文件里
// （turn_context 的 payload.model），而原来的代码把它挂在「用量走了文件那条路」的
// 分支上——一旦 JSON-RPC 通知报了用量，那条分支就不走，模型名永远是空的。
//
// 真机证据：3/3 条 codex 用量的 model 都是 `unknown`，而它们各自的 rollout 文件里
// 都清清楚楚写着 turn_context 的 payload.model = "gpt-5.6-sol"。
//
// 这个后果比「显示不好看」严重：`unknown` 在四态成本口径里落到 `unpriced`，既不能
// 显示金额，也不能进中位数与预算——一条跑了 23 万 cache read 的 Run 在成本上等于
// 完全不存在。

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeRuelCodexRollout 造一个 codex rollout 文件，形状与真机一致：
// 一条 session_meta（带 thread id）+ 若干事件。
//
// withTokenCount 控制有没有 token_count 事件——这正是两条路径分离的关键：
// 没有 token_count 时 scanCodexSessionUsage 会整体放弃（返回 nil），但模型名
// **仍然在 turn_context 里**，必须照样读得出来。
func writeRuelCodexRollout(t *testing.T, dir, threadID, model string, withTokenCount bool) string {
	t.Helper()
	sessions := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}
	path := filepath.Join(sessions, "rollout-2026-10-07T09-36-49-"+threadID+".jsonl")
	var out string
	out += `{"timestamp":"2026-10-07T01:36:49.000Z","type":"session_meta","payload":{"id":"` + threadID + `","cwd":"/tmp"}}` + "\n"
	out += `{"timestamp":"2026-10-07T01:36:50.000Z","type":"turn_context","payload":{"model":"` + model + `"}}` + "\n"
	if withTokenCount {
		out += `{"timestamp":"2026-10-07T01:36:51.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":16339,"output_tokens":1209,"cached_input_tokens":235648},"last_token_usage":{"input_tokens":16339,"output_tokens":1209,"cached_input_tokens":235648}}}}` + "\n"
	}
	out += `{"timestamp":"2026-10-07T01:36:52.000Z","type":"event_msg","payload":{"type":"task_complete"}}` + "\n"
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	return path
}

// TestCodexSessionModelReadsTurnContext 守的是「模型名读得到」。
//
// 它单独一条，是因为这里最容易写错的是**字段路径**：turn_context 的模型在
// payload.model，而 token_count 的在 payload.info.model，两者不在同一层。
func TestCodexSessionModelReadsTurnContext(t *testing.T) {
	dir := t.TempDir()
	threadID := "01a11401-92eb-7393-b2ab-e6bf00813ba9"
	// 故意只放 turn_context、不放 token_count：最纯粹的形态。
	writeRuelCodexRollout(t, dir, threadID, "gpt-5.6-sol", false)

	if got := codexSessionModel(time.Time{}, dir, threadID); got != "gpt-5.6-sol" {
		t.Fatalf("codexSessionModel = %q, want %q", got, "gpt-5.6-sol")
	}
}

// TestCodexSessionModelIndependentOfUsagePath 守的是**根因**：模型名不能挂在用量的
// 分支上。
//
// 两种形态都要读得到模型：
//   - 有 token_count（用量走文件那条路，旧代码也能拿到）
//   - 没有 token_count（scanCodexSessionUsage 会返回 nil，旧代码在这里丢掉模型名）
//
// 第二条是真机上 3/3 条用量变成 unknown 的直接原因——不是模型名不存在，是那条分支
// 压根没走。
func TestCodexSessionModelIndependentOfUsagePath(t *testing.T) {
	dir := t.TempDir()
	threadID := "01a11404-6cab-7e13-8eec-3a725a657246"
	writeRuelCodexRollout(t, dir, threadID, "gpt-5.6-sol", true)
	if got := codexSessionModel(time.Time{}, dir, threadID); got != "gpt-5.6-sol" {
		t.Fatalf("有 token_count 时 codexSessionModel = %q, want %q", got, "gpt-5.6-sol")
	}

	dir2 := t.TempDir()
	threadID2 := "01a11404-6cab-7e13-8eec-3a725a657247"
	writeRuelCodexRollout(t, dir2, threadID2, "gpt-5.6-sol", false)
	// 同一份文件，用量那条路应当**放弃**（没有 token_count）——这正是要证明的前提：
	// 用量拿不到，模型照样拿得到。
	if u := scanCodexSessionUsage(time.Time{}, dir2, threadID2, false); u != nil {
		t.Fatalf("scanCodexSessionUsage 应当因为没有用量而放弃，实际 = %+v", u)
	}
	if got := codexSessionModel(time.Time{}, dir2, threadID2); got != "gpt-5.6-sol" {
		t.Fatalf("没有 token_count 时 codexSessionModel = %q, want %q", got, "gpt-5.6-sol")
	}
}

// TestCodexSessionModelTakesTheLastModel 守的是「一次 Run 里换过模型」时取哪个。
//
// 取最后一个：用量是累计的，它对应的是**最后**那个模型的价格结构。取第一个会让一条
// 用量的钱按前半程的单价算，静默地错。
func TestCodexSessionModelTakesTheLastModel(t *testing.T) {
	dir := t.TempDir()
	threadID := "01a11405-0000-0000-0000-000000000001"
	path := writeRuelCodexRollout(t, dir, threadID, "gpt-5.6-sol", false)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("reopen rollout: %v", err)
	}
	if _, err := f.WriteString(`{"timestamp":"2026-10-07T01:37:10.000Z","type":"turn_context","payload":{"model":"gpt-5.6-terra"}}` + "\n"); err != nil {
		t.Fatalf("append rollout: %v", err)
	}
	f.Close()

	if got := codexSessionModel(time.Time{}, dir, threadID); got != "gpt-5.6-terra" {
		t.Fatalf("换过模型后 codexSessionModel = %q, want 最后一个 %q", got, "gpt-5.6-terra")
	}
}

// TestCodexSessionModelMissingInputsAreEmpty 守的是「读不出来就返回空，不编一个数」。
//
// 返回空最终会落到字面量 `unknown`，再落到四态口径里的 `unpriced`。那是对的：
// 不知道就是不知道，编一个模型名会让一条用量按错误的单价算钱，而且不会报错。
func TestCodexSessionModelMissingInputsAreEmpty(t *testing.T) {
	if got := codexSessionModel(time.Time{}, "", "some-thread"); got != "" {
		t.Fatalf("空 CODEX_HOME: codexSessionModel = %q, want 空", got)
	}
	if got := codexSessionModel(time.Time{}, t.TempDir(), ""); got != "" {
		t.Fatalf("空 threadID: codexSessionModel = %q, want 空", got)
	}
	if got := codexSessionModel(time.Time{}, t.TempDir(), "no-such-thread"); got != "" {
		t.Fatalf("没有对应 rollout: codexSessionModel = %q, want 空", got)
	}
}
