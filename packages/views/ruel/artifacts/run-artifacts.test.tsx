// @vitest-environment jsdom

import { fireEvent, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { describe, expect, it } from "vitest";
import type { RuelArtifact } from "@multica/core/ruel/artifacts";
import { ruelArtifactKeys } from "@multica/core/ruel/queries";
import { renderWithI18n } from "../../test/i18n";

// 这份 fixture 直接抄数据库里那条真实记录（RUEL-8 那一轮 Run）：一个 intent-to-add
// 的新文件。用真实形状而不是随手编的，是因为解析和渲染都依赖采集侧的格式，格式变了
// 这里应该先炸。
const DIFF_ONE = [
  "diff --git a/greeting.txt b/greeting.txt",
  "new file mode 100644",
  "--- /dev/null",
  "+++ b/greeting.txt",
  "@@ -0,0 +1 @@",
  "+hello from ruel",
].join("\n");

const DIFF_TWO = [
  "diff --git a/greeting.txt b/greeting.txt",
  "--- a/greeting.txt",
  "+++ b/greeting.txt",
  "@@ -1 +1 @@",
  "-hello from ruel",
  "+hello again",
].join("\n");

function artifact(over: Partial<RuelArtifact> & { task_id: string; kind: string }): RuelArtifact {
  return {
    id: `${over.task_id}-${over.kind}`,
    uri: "workdir://C:/work/repo",
    size: 0,
    checksum: "",
    content: "",
    created_at: "2026-10-02T02:56:32Z",
    ...over,
  };
}

const ARTIFACTS: RuelArtifact[] = [
  // 第 1 轮：新建文件
  artifact({ task_id: "task-1", kind: "file_change", content: " A greeting.txt\n" }),
  artifact({ task_id: "task-1", kind: "diff_stat", content: " greeting.txt | 1 +\n 1 file changed, 1 insertion(+)\n" }),
  artifact({ task_id: "task-1", kind: "diff", content: DIFF_ONE, checksum: "aaaaaaaa" }),
  // 第 2 轮：改掉那行。只看最后一轮的话，第 1 轮的「新建」就不见了。
  artifact({ task_id: "task-2", kind: "file_change", content: " M greeting.txt\n", created_at: "2026-10-02T03:10:00Z" }),
  artifact({ task_id: "task-2", kind: "diff_stat", content: " greeting.txt | 2 +-\n", created_at: "2026-10-02T03:10:00Z" }),
  artifact({ task_id: "task-2", kind: "diff", content: DIFF_TWO, checksum: "bbbbbbbb", created_at: "2026-10-02T03:10:00Z" }),
];

function renderWithArtifacts(ui: React.ReactElement, artifacts: RuelArtifact[]) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: Infinity } },
  });
  client.setQueryData(ruelArtifactKeys.issue("issue-1"), artifacts);
  return renderWithI18n(<QueryClientProvider client={client}>{ui}</QueryClientProvider>, {
    locale: "zh-Hans",
  });
}

import { RunArtifactsPanel } from "./run-artifacts";
import { IssueArtifactsSection } from "./issue-artifacts-section";

describe("RunArtifactsPanel", () => {
  it("折叠状态下给出文件数与加减行数", () => {
    renderWithArtifacts(<RunArtifactsPanel issueId="issue-1" taskId="task-1" />, ARTIFACTS);
    expect(screen.getByText("本次 Run 的变更")).toBeInTheDocument();
    expect(screen.getByText("1 个文件")).toBeInTheDocument();
    expect(screen.getByText("+1")).toBeInTheDocument();
  });

  it("展开后能看到变更文件列表与 diff 内容", () => {
    renderWithArtifacts(<RunArtifactsPanel issueId="issue-1" taskId="task-1" />, ARTIFACTS);
    fireEvent.click(screen.getByText("本次 Run 的变更"));
    expect(screen.getAllByText("greeting.txt").length).toBeGreaterThan(0);
    expect(screen.getByText("hello from ruel")).toBeInTheDocument();
  });

  it("这一轮没有产物时整块不渲染，而不是显示一个空区块", () => {
    const { container } = renderWithArtifacts(
      <RunArtifactsPanel issueId="issue-1" taskId="task-none" />,
      ARTIFACTS,
    );
    expect(container.textContent).toBe("");
  });
});

describe("IssueArtifactsSection", () => {
  it("逐轮列出，而不是只显示最后一轮", () => {
    renderWithArtifacts(<IssueArtifactsSection issueId="issue-1" />, ARTIFACTS);
    expect(screen.getByText("第 1 轮")).toBeInTheDocument();
    expect(screen.getByText("第 2 轮")).toBeInTheDocument();
  });

  it("表头的累计文件数按去重后的路径算", () => {
    renderWithArtifacts(<IssueArtifactsSection issueId="issue-1" />, ARTIFACTS);
    // 两轮动的是同一个文件：表头的累计口径算 1 个文件而不是 2 次改动，但每一轮仍然
    // 各显示自己的 1 个文件——所以是表头 1 处 + 两轮各 1 处。
    expect(screen.getAllByText("1 个文件")).toHaveLength(3);
  });

  it("没有产物时整块不渲染", () => {
    const { container } = renderWithArtifacts(
      <IssueArtifactsSection issueId="issue-1" />,
      [],
    );
    expect(container.textContent).toBe("");
  });
});
