package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/ruel/artifacts"
)

// 这组测试盯着一个已经踩过一次的坑：work_dir 是**容器目录**，真正的 git 仓库在它的
// 子目录里。对容器目录直接跑 git 会静默失败，产物为空——表面上像「这个 Run 没有
// 变更」，实际是采集逻辑找错了地方。测试把「定位」和「采集」两件事分开验，
// 定位错了要能在第一层就被发现，而不是等到数据库里没数据。

func TestRuelResolveGitDirs(t *testing.T) {
	t.Run("work_dir 自带 .git 时直接用它", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		got := ruelResolveGitDirs(root)
		if len(got) != 1 || got[0] != root {
			t.Fatalf("期望 [%s]，实际 %v", root, got)
		}
	})

	t.Run("真正的仓库在子目录里", func(t *testing.T) {
		// 复刻上游实测布局：<work_dir>/CLAUDE.md + <work_dir>/<repo>/.git
		workDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(workDir, "CLAUDE.md"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(workDir, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		repo := filepath.Join(workDir, "ruel-smoke-fixture")
		if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}

		got := ruelResolveGitDirs(workDir)
		if len(got) != 1 || got[0] != repo {
			t.Fatalf("期望定位到子仓库 [%s]，实际 %v", repo, got)
		}
	})

	t.Run("多个子仓库都要覆盖", func(t *testing.T) {
		workDir := t.TempDir()
		var want []string
		for _, name := range []string{"alpha", "beta"} {
			repo := filepath.Join(workDir, name)
			if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			want = append(want, repo)
		}
		// 干扰项：非仓库目录、隐藏目录都不该进来
		if err := os.MkdirAll(filepath.Join(workDir, "node_modules", "pkg"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(workDir, ".hidden", ".git"), 0o755); err != nil {
			t.Fatal(err)
		}

		got := ruelResolveGitDirs(workDir)
		if len(got) != len(want) {
			t.Fatalf("期望 %d 个仓库，实际 %d：%v", len(want), len(got), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("第 %d 个：期望 %s，实际 %s", i, want[i], got[i])
			}
		}
	})

	t.Run("找不到仓库时退回 work_dir 本身", func(t *testing.T) {
		workDir := t.TempDir()
		got := ruelResolveGitDirs(workDir)
		if len(got) != 1 || got[0] != workDir {
			t.Fatalf("期望退回 [%s]，实际 %v", workDir, got)
		}
	})
}

// TestCollectRuelArtifactsOnContainerLayout 是那个 bug 的回归测试：在容器布局下，
// 必须能采到子仓库里的变更。修之前这里返回空切片。
func TestCollectRuelArtifactsOnContainerLayout(t *testing.T) {
	git := testRuelFindGit(t)

	workDir := t.TempDir()
	repo := filepath.Join(workDir, "fixture")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	testRuelGit(t, repo, git, "init", "-q")
	testRuelGit(t, repo, git, "config", "user.email", "ruel@example.com")
	testRuelGit(t, repo, git, "config", "user.name", "Ruel")
	testRuelGit(t, repo, git, "config", "commit.gpgsign", "false")

	// 一个已提交的基线文件，用来产生真正的 diff（而不是只有 untracked）
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testRuelGit(t, repo, git, "add", ".")
	testRuelGit(t, repo, git, "commit", "-q", "-m", "baseline")

	// Agent 的两类典型动作：改已有文件 + 新增文件
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := collectRuelArtifacts(workDir)
	if len(got.Items) == 0 {
		t.Fatal("容器布局下应当采到产物，实际为空——ruelResolveGitDirs 没定位到子仓库")
	}
	if got.Status != artifacts.StatusChanged {
		t.Errorf("采到产物时结论应当是 changed，实际 %s", got.Status)
	}

	byKind := map[string]string{}
	for _, p := range got.Items {
		byKind[p.Kind] = p.Content
		if p.URI != "workdir://"+repo {
			t.Errorf("kind=%s 的 URI 指向了错误目录：%s", p.Kind, p.URI)
		}
	}

	if diff := byKind["diff"]; !strings.Contains(diff, "+world") {
		t.Errorf("diff 里应当包含新增行，实际：\n%s", diff)
	}
	if stat := byKind["diff_stat"]; !strings.Contains(stat, "README.md") {
		t.Errorf("diff_stat 里应当列出改动文件，实际：\n%s", stat)
	}
	if fc := byKind["file_change"]; !strings.Contains(fc, "feature.txt") {
		t.Errorf("file_change 里应当包含新文件，实际：\n%s", fc)
	}
}

// TestCollectRuelArtifactsIncludesNewFiles 是最要紧的一条回归测试。
//
// agent 最典型的产出是新建文件，而 `git diff` 默认只统计已跟踪文件——只看 diff 的
// 话，「agent 新建了一个文件」和「agent 什么都没干」长得一模一样。产物里写着无 diff，
// Issue 页上就变成了一次空转的 Run。
func TestCollectRuelArtifactsIncludesNewFiles(t *testing.T) {
	git := testRuelFindGit(t)

	repo := t.TempDir()
	testRuelGit(t, repo, git, "init", "-q")
	testRuelGit(t, repo, git, "config", "user.email", "ruel@example.com")
	testRuelGit(t, repo, git, "config", "user.name", "Ruel")
	testRuelGit(t, repo, git, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testRuelGit(t, repo, git, "add", ".")
	testRuelGit(t, repo, git, "commit", "-q", "-m", "baseline")

	// 只新建文件，不动任何已跟踪文件
	if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("new file\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var diff, stat string
	for _, p := range collectRuelArtifacts(repo).Items {
		switch p.Kind {
		case "diff":
			diff = p.Content
		case "diff_stat":
			stat = p.Content
		}
	}
	if !strings.Contains(diff, "feature.txt") {
		t.Errorf("新建文件必须出现在 diff 里，实际：\n%s", diff)
	}
	if !strings.Contains(stat, "feature.txt") {
		t.Errorf("新建文件必须出现在 diff_stat 里，实际：\n%s", stat)
	}
}

// TestCollectRuelArtifactsIncludesCommittedWork 验基线口径：agent 把改动提交掉之后，
// 产物里仍然要能看到这次 Run 改了什么。
//
// 用 HEAD 当基线时这条必然失败——提交之后工作区是干净的，diff 为空。
func TestCollectRuelArtifactsIncludesCommittedWork(t *testing.T) {
	git := testRuelFindGit(t)

	// 一个裸仓库充当远端，这样 refs/remotes/origin/HEAD 才存在
	origin := filepath.Join(t.TempDir(), "origin.git")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	// 显式指定默认分支：裸仓库 HEAD 的默认值随 git 版本和全局配置而变，写死才能
	// 让 refs/remotes/origin/HEAD 稳定解析出来。
	testRuelGit(t, origin, git, "init", "-q", "--bare", "-b", "main")

	repo := filepath.Join(t.TempDir(), "work")
	testRuelGit(t, t.TempDir(), git, "clone", "-q", origin, repo)
	testRuelGit(t, repo, git, "config", "user.email", "ruel@example.com")
	testRuelGit(t, repo, git, "config", "user.name", "Ruel")
	testRuelGit(t, repo, git, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testRuelGit(t, repo, git, "add", ".")
	testRuelGit(t, repo, git, "commit", "-q", "-m", "init")
	testRuelGit(t, repo, git, "push", "-q", "-u", "origin", "HEAD:main")
	testRuelGit(t, repo, git, "remote", "set-head", "origin", "--auto")

	// agent 在任务分支上提交
	testRuelGit(t, repo, git, "checkout", "-q", "-b", "agent/m3-smoke/abc123")
	if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("agent 写的\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testRuelGit(t, repo, git, "add", ".")
	testRuelGit(t, repo, git, "commit", "-q", "-m", "feat: 加个功能")

	var diff string
	for _, p := range collectRuelArtifacts(repo).Items {
		if p.Kind == "diff" {
			diff = p.Content
		}
	}
	if !strings.Contains(diff, "feature.txt") {
		t.Errorf("已提交的改动必须出现在 diff 里，实际：\n%s", diff)
	}
	if !strings.Contains(diff, "agent 写的") {
		t.Errorf("diff 里要能看到提交的内容，实际：\n%s", diff)
	}
}

// TestCollectRuelArtifactsTruncates 验截断：diff 超过上限时保留头段并留下标记，
// 不静默丢掉。
func TestCollectRuelArtifactsTruncates(t *testing.T) {
	git := testRuelFindGit(t)

	repo := t.TempDir()
	testRuelGit(t, repo, git, "init", "-q")
	testRuelGit(t, repo, git, "config", "user.email", "ruel@example.com")
	testRuelGit(t, repo, git, "config", "user.name", "Ruel")
	testRuelGit(t, repo, git, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "big.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testRuelGit(t, repo, git, "add", ".")
	testRuelGit(t, repo, git, "commit", "-q", "-m", "baseline")
	if err := os.WriteFile(filepath.Join(repo, "big.txt"),
		[]byte(strings.Repeat("0123456789\n", 200000)), 0o644); err != nil {
		t.Fatal(err)
	}

	var diff string
	for _, p := range collectRuelArtifacts(repo).Items {
		if p.Kind == "diff" {
			diff = p.Content
		}
	}
	if diff == "" {
		t.Fatal("应当采到 diff")
	}
	if !strings.Contains(diff, "[ruel] 内容超过") {
		t.Error("超限时应当留下截断标记，否则读的人会以为看到的是完整 diff")
	}
	if len(diff) < ruelArtifactMaxBytes {
		t.Errorf("截断后长度 %d 应当接近上限 %d", len(diff), ruelArtifactMaxBytes)
	}
}

// TestCollectRuelArtifactsStatus 锁住「这一轮没有产物」的三种情况必须分开。
//
// 这是 #17 的核心。产物表为空时，界面上只有一句「变更：无」，而它底下压着三种性质
// 完全不同的事：没碰仓库（正常的一类 Run）、碰了但没改动（正常）、采集失败（缺陷）。
// 前两种正常、第三种是缺陷，可它们在库里长得一模一样——混在一起，缺陷就被伪装成了
// 「这轮没干活」。
func TestCollectRuelArtifactsStatus(t *testing.T) {
	git := testRuelFindGit(t)

	t.Run("没碰仓库：agent 只在容器目录里写了东西", func(t *testing.T) {
		// 复刻 RUEL-7 的真实情形：容器目录里有产出，但没有 repo checkout。
		workDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(workDir, "notes.md"), []byte("调研结论"), 0o644); err != nil {
			t.Fatal(err)
		}
		got := collectRuelArtifacts(workDir)
		if got.Status != artifacts.StatusNoRepo {
			t.Fatalf("应当是 %s，实际 %s（%s）", artifacts.StatusNoRepo, got.Status, got.Diagnostic)
		}
		if len(got.Items) != 0 {
			t.Errorf("没碰仓库时不该产出任何产物，实际 %d 条", len(got.Items))
		}
	})

	// 连工作目录都没有 ≠ 采集到了「没有仓库」。前者是这次压根没采集，结论行必须缺席
	// （前端的第四种状态就是「没有结论行」），把它记成 no_repo 是在编造一次采集。
	t.Run("没有工作目录：这次不采集，不给结论", func(t *testing.T) {
		for _, dir := range []string{"", "  ", filepath.Join(t.TempDir(), "不存在")} {
			got := collectRuelArtifacts(dir)
			if got.Status != "" {
				t.Errorf("work_dir %q 不该给出结论，实际 %s（%s）", dir, got.Status, got.Diagnostic)
			}
			if len(got.Items) != 0 {
				t.Errorf("work_dir %q 不该产出任何产物，实际 %d 条", dir, len(got.Items))
			}
		}
	})

	t.Run("碰了但没改动：只读任务", func(t *testing.T) {
		repo := t.TempDir()
		testRuelGit(t, repo, git, "init", "-q")
		testRuelGit(t, repo, git, "config", "user.email", "ruel@example.com")
		testRuelGit(t, repo, git, "config", "user.name", "Ruel")
		testRuelGit(t, repo, git, "config", "commit.gpgsign", "false")
		if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		testRuelGit(t, repo, git, "add", ".")
		testRuelGit(t, repo, git, "commit", "-q", "-m", "baseline")

		got := collectRuelArtifacts(repo)
		if got.Status != artifacts.StatusNoChange {
			t.Fatalf("干净仓库应当是 %s，实际 %s（%s）", artifacts.StatusNoChange, got.Status, got.Diagnostic)
		}
		// 判据三：只读任务不产生误导性的空产物记录。写一条 content 为空的 diff 进去，
		// 界面上就会显示一个「变更」区块、点开是空的——那比什么都不显示更糟。
		if len(got.Items) != 0 {
			t.Errorf("只读任务不该写入任何产物记录，实际 %d 条：%+v", len(got.Items), got.Items)
		}
	})

	t.Run("采集失败：仓库在，但 git 跑不出来", func(t *testing.T) {
		// .git 是个空目录：定位阶段能找到它（.git 存在），但任何 git 命令都会失败。
		// 这正对应 git 二进制缺失 / 权限问题 / 仓库损坏那类缺陷。
		workDir := t.TempDir()
		repo := filepath.Join(workDir, "broken")
		if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "src.txt"), []byte("改过了\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		got := collectRuelArtifacts(workDir)
		if got.Status != artifacts.StatusCollectFailed {
			t.Fatalf("应当是 %s，实际 %s", artifacts.StatusCollectFailed, got.Status)
		}
		if got.Diagnostic == "" {
			t.Error("采集失败要带上诊断信息，否则只知道失败、无从排查")
		}
		if len(got.Items) != 0 {
			t.Errorf("采集失败时不该有产物，实际 %d 条", len(got.Items))
		}
	})
}

func testRuelFindGit(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("git"); err == nil {
		return p
	}
	t.Skip("环境里没有 git，跳过依赖 git 的用例")
	return ""
}

// testRuelGit 用显式指定的 git 二进制，不依赖 PATH——daemon 跑在用户的机器上，
// PATH 里未必有 git，这条路径本身就值得在测试里走一遍。
func testRuelGit(t *testing.T, dir, git string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, git, append([]string{"-C", dir}, args...)...)
	buf, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v 失败：%v\n%s", args, err, buf)
	}
}
