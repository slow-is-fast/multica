package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/ruel/artifacts"
)

// Ruel 新增：Run 结束后采集 worktree 的变更，作为 P0-6 的产物证据上报。
//
// 上游的完成回调只带 output / pr_url / work_dir / session_id——没有 diff，也没有测试
// 证据。Issue 页上那句「测试通过」因此无从核对，验收只能建立在信任声明上。这里补上
// 采集，落点见 internal/ruel/artifacts。
//
// 两条刻意的取舍：
//
//  1. 采集在**上报完成之前**做，上报在完成之后做。work_dir 可能在任务终态后被回收，
//     先把内容读进内存才不会拿到空目录。
//  2. 产物上报失败**不阻塞、不回滚**完成状态。完成是硬性状态，产物是附加证据——
//     为了附加证据去回滚一个已经正确的终态，是本末倒置。失败只记日志。

// ruelArtifactMaxBytes 是单条产物的上限。diff 可以非常大，原样上报会把数据库和
// 网络都压垮；截断并留标记，比静默丢掉或撑爆都好。
const ruelArtifactMaxBytes = 1 << 20 // 1 MiB

// ruelArtifactPayload 与服务端 RuelArtifactRequest 对应。checksum 不由这里计算——
// 服务端用同一份 content 重算，客户端自报的校验值没有意义。
type ruelArtifactPayload struct {
	Kind    string `json:"kind"`
	Content string `json:"content"`
	URI     string `json:"uri"`
}

// collectRuelArtifacts 读出一个 worktree 的变更。返回空切片表示「确实没有变更」或
// 「采集不出来」，两种情况都不算缺陷——真正的缺陷是「有变更但没采集」，那要记日志。
func collectRuelArtifacts(workDir string) []ruelArtifactPayload {
	workDir = strings.TrimSpace(workDir)
	if workDir == "" {
		return nil
	}
	if _, err := os.Stat(workDir); err != nil {
		return nil
	}

	specs := []struct {
		kind string
		args []string
	}{
		{artifacts.KindDiff, []string{"diff", "HEAD"}},
		{artifacts.KindDiffStat, []string{"diff", "HEAD", "--stat"}},
		{artifacts.KindFileChange, []string{"status", "--porcelain"}},
	}

	out := make([]ruelArtifactPayload, 0, len(specs))
	for _, spec := range specs {
		content, err := ruelGitOutput(workDir, spec.args...)
		if err != nil {
			// git 失败（不是 git 仓库、二进制缺失）不该让任务失败，跳过即可。
			continue
		}
		if strings.TrimSpace(content) == "" {
			// 这个维度确实没有变更。与「采集不到」区分开：空是合法结果。
			continue
		}
		if len(content) > ruelArtifactMaxBytes {
			content = content[:ruelArtifactMaxBytes] +
				fmt.Sprintf("\n\n[ruel] 内容超过 %d 字节已截断，完整 diff 请到 %s 查看\n",
					ruelArtifactMaxBytes, workDir)
		}
		out = append(out, ruelArtifactPayload{
			Kind:    spec.kind,
			Content: content,
			URI:     "workdir://" + workDir,
		})
	}
	return out
}

func ruelGitOutput(workDir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", workDir}, args...)...)
	buf, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(buf), nil
}

// reportRuelArtifact 上报一条产物。
func (c *Client) reportRuelArtifact(ctx context.Context, taskID string, payload ruelArtifactPayload) error {
	return c.postJSONWithRetry(ctx, fmt.Sprintf("/api/daemon/tasks/%s/artifacts", taskID), payload, nil, nil)
}
