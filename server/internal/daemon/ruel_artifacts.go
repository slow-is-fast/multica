package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
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

	out := make([]ruelArtifactPayload, 0, 3)
	for _, repo := range ruelResolveGitDirs(workDir) {
		if !ruelIsGitRepo(repo) {
			continue
		}
		if err := ruelMarkIntentToAdd(repo); err != nil {
			// 标记失败不致命：退化成「只统计已跟踪文件的改动」，总比什么都不记好。
			slog.Warn("ruel: 标记未跟踪文件失败，diff 可能缺少新增文件",
				"repo", repo, "error", err)
		}
		// 基线取分支的派生点，而不是 HEAD。
		//
		// 用 HEAD 会把「agent 提交了代码」这种情况算成零改动——提交之后工作区是干净的，
		// `git diff HEAD` 自然为空，于是产物里写着「无 diff」，而 agent 其实写了一个
		// 功能。派生点版本的 `git diff <base>` 同时覆盖已提交和未提交两部分，才是
		// 「这次 Run 到底改了什么」的正确答案。
		base := ruelGitBase(repo)

		specs := []struct {
			kind string
			args []string
		}{
			{artifacts.KindDiff, []string{"diff", base}},
			{artifacts.KindDiffStat, []string{"diff", base, "--stat"}},
			{artifacts.KindFileChange, []string{"status", "--porcelain"}},
		}

		for _, spec := range specs {
			content, err := ruelGitOutput(repo, spec.args...)
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
						ruelArtifactMaxBytes, repo)
			}
			out = append(out, ruelArtifactPayload{
				Kind:    spec.kind,
				Content: content,
				URI:     "workdir://" + repo,
			})
		}
	}
	return out
}

// ruelResolveGitDirs 找出真正含 .git 的目录。
//
// 这一步不能省。上游传下来的 work_dir 是**容器目录**而不是仓库根：它里面放着
// CLAUDE.md、.claude、.multica 这些托管文件，真正的 checkout 在同级的子目录里
// （实测结构：<work_dir>/<repo-name>/.git）。直接对 work_dir 跑 git 只会得到
// "not a git repository"，产物静默为空——表面看是「没有变更」，实际是采集逻辑
// 找错了地方。
//
// 只向下看一层，不做全盘搜索：容器布局是固定的，递归遍历既慢又可能扫进 node_modules。
// 找得到就用找到的，找不到就退回 work_dir 本身（让 git 自己报错）。
func ruelResolveGitDirs(workDir string) []string {
	if ruelIsGitRepo(workDir) {
		return []string{workDir}
	}
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return []string{workDir}
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p := filepath.Join(workDir, e.Name())
		if ruelIsGitRepo(p) {
			dirs = append(dirs, p)
		}
	}
	if len(dirs) == 0 {
		return []string{workDir}
	}
	return dirs
}

func ruelIsGitRepo(dir string) bool {
	// git worktree 的 .git 是文件，普通仓库是目录，两种都要认。
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return true
	}
	if _, err := ruelGitOutput(dir, "rev-parse", "--git-dir"); err == nil {
		return true
	}
	return false
}

// ruelMarkIntentToAdd 把未跟踪文件标记为 intent-to-add，让它们进入 diff。
//
// 这是 P0-6 最容易踩空的一处。agent 最典型的产出就是**新建文件**——加一个测试、
// 加一个模块。而 `git diff` 默认只看已跟踪文件，新建的文件连一行都不会出现在
// diff 里。结果就是：agent 明明写了东西，产物里却写着「无 diff」，Issue 页上看着
// 像这轮 Run 什么都没干。
//
// 代价是动了一次索引。`git add -N` 只登记「打算加入」，不暂存内容，也不改变工作区
// 文件；对一个任务跑完就废弃的 worktree 来说，这份索引改动无关紧要，而漏掉新增文件
// 会让整条产物链失去意义。两者不在一个量级上。
func ruelMarkIntentToAdd(repo string) error {
	_, err := ruelGitOutput(repo, "add", "-N", ".")
	return err
}

// ruelGitBase 求分支的派生点，作为「这次 Run 改了什么」的基线。
//
// 优先用远端默认分支（refs/remotes/origin/HEAD，clone 时 git 自己建好）与 HEAD 的
// merge-base：分支正是从那里长出来的，diff 出来的就是本次任务的净增量。
//
// 拿不到时退回 HEAD——那时 diff 退化成「未提交的改动」，会漏掉 agent 已提交的部分，
// 但不至于整个采集失败。派生点算不出来通常意味着这是个没有远端的孤立仓库，本来也
// 没有「基线」可言。
func ruelGitBase(repo string) string {
	if remote, err := ruelGitOutput(repo, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/HEAD"); err == nil {
		remote = strings.TrimSpace(remote)
		if remote != "" {
			if base, err := ruelGitOutput(repo, "merge-base", remote, "HEAD"); err == nil {
				base = strings.TrimSpace(base)
				if base != "" {
					return base
				}
			}
		}
	}
	return "HEAD"
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
