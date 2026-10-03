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

// ruelArtifactStatusPayload 与服务端 RuelArtifactStatusRequest 对应。
type ruelArtifactStatusPayload struct {
	Status     string `json:"status"`
	Diagnostic string `json:"diagnostic"`
}

// ruelArtifactCollection 是一次采集的结果：采到了什么，以及没采到时是为什么。
//
// 之所以要带结论，是因为「产物表为空」这件事本身有三种截然不同的含义，而它们在
// 数据库里长得一模一样：
//
//  1. no_repo——这次 Run 根本没碰代码仓库。agent 只在容器目录里写了东西（调研、
//     读文档、写方案），没有触发 repo checkout。这是正常的一类 Run。
//  2. no_change——碰了仓库，但确实没改动。只读任务，或者改动被 revert 回去了。
//     这也是正常的。
//  3. collect_failed——git 缺失、超时、权限问题。这是缺陷。
//
// 前两种正常、第三种是缺陷，可界面上都是「变更：无」。把结论一并存下来，界面才说得
// 出「这轮为什么没有产物」，而不是让缺陷混在正常里一起被当成没干活。
type ruelArtifactCollection struct {
	Status     string
	Diagnostic string
	Items      []ruelArtifactPayload
}

// collectRuelArtifacts 读出一个 worktree 的变更，并给出这次采集的结论。
func collectRuelArtifacts(workDir string) ruelArtifactCollection {
	workDir = strings.TrimSpace(workDir)
	if workDir == "" {
		return ruelArtifactCollection{Status: artifacts.StatusNoRepo, Diagnostic: "work_dir 为空"}
	}
	if _, err := os.Stat(workDir); err != nil {
		return ruelArtifactCollection{Status: artifacts.StatusNoRepo, Diagnostic: "work_dir 不可访问"}
	}

	// 先定位、再判断结论：定位不到仓库与「仓库里没改动」是两件事，不能都归成空。
	repos := make([]string, 0, 1)
	for _, repo := range ruelResolveGitDirs(workDir) {
		if ruelIsGitRepo(repo) {
			repos = append(repos, repo)
		}
	}
	if len(repos) == 0 {
		return ruelArtifactCollection{
			Status:     artifacts.StatusNoRepo,
			Diagnostic: "work_dir 下没有 git 仓库",
		}
	}

	out := make([]ruelArtifactPayload, 0, 3)
	failed := 0
	var firstErr string
	for _, repo := range repos {
		if err := ruelMarkIntentToAdd(repo); err != nil {
			// 标记失败不致命：退化成「只统计已跟踪文件的改动」，总比什么都不记好。
			// 也不计入 failed——它是优化项，失败了仍有可用的采集结果。
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
				// git 失败（二进制缺失、权限、超时）不该让任务失败，但必须留下痕迹：
				// 「跑不出来」和「跑出来是空的」是两回事，前者是缺陷。
				failed++
				if firstErr == "" {
					firstErr = fmt.Sprintf("%s: %v", spec.kind, err)
				}
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

	switch {
	case len(out) > 0:
		// 部分维度失败但采到了东西：产物仍然可用，缺陷降级为日志。
		if failed > 0 {
			slog.Warn("ruel: 部分产物采集失败，该 Run 的产物不完整",
				"failed", failed, "first_error", firstErr)
		}
		return ruelArtifactCollection{Status: artifacts.StatusChanged, Items: out}
	case failed > 0:
		return ruelArtifactCollection{
			Status:     artifacts.StatusCollectFailed,
			Diagnostic: firstErr,
		}
	default:
		// 命令都跑成功了，输出都是空的：这轮确实没改动，不写任何空产物记录。
		return ruelArtifactCollection{Status: artifacts.StatusNoChange}
	}
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

// reportRuelArtifactStatus 上报一次采集的结论。
//
// 结论要在产物之后上报，且即使这轮没有任何产物也要上报——「没有产物」与「没采集」
// 的区分全靠这一条。它失败时同样只记日志：完成状态已经落地，不该为了结论回滚终态。
func (c *Client) reportRuelArtifactStatus(
	ctx context.Context,
	taskID string,
	payload ruelArtifactStatusPayload,
) error {
	return c.postJSONWithRetry(ctx, fmt.Sprintf("/api/daemon/tasks/%s/artifact-status", taskID), payload, nil, nil)
}
