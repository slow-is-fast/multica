// @vitest-environment jsdom

// #42 的按钮该不该出现，是这条 Issue 里最容易做错也最难靠肉眼发现的一半。
//
// 判据是 attempt_count，不是 status：两类失败都会落到 turn_ended，只按 status 或
// failure_reason 判断就一定会在某一类上出错。四种回执形态逐个渲染一遍，比在真机上
// 截一张图更准——真机截一次只能证明「当时那一条显示对了」。
//
// 尤其第三例：老服务端不发 attempt_count。undefined 是「不知道」而不是「从未投递」，
// 按 0 处理就会显示一个点了也不生效的按钮——承诺一件做不到的事比不承诺更糟。

import { describe, it, expect, vi, afterEach } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import type { TimelineEntry } from "@multica/core/types";
import { SteerReceipts } from "./steer-receipts";
import zhCommon from "../../locales/zh-Hans/common.json";
import zhIssues from "../../locales/zh-Hans/issues.json";

const TEST_RESOURCES = { "zh-Hans": { common: zhCommon, issues: zhIssues } };

vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({ getActorName: () => "Lambda" }),
}));
vi.mock("@multica/core/issues/queries", () => ({
  issueKeys: { timeline: (id: string) => ["timeline", id], tasks: (id: string) => ["tasks", id] },
  issueTasksOptions: () => ({ queryKey: ["tasks"], queryFn: async () => [] }),
}));
vi.mock("@multica/core/issues/mutations", () => ({
  useCreateComment: () => ({ mutate: vi.fn(), isPending: false }),
  useRetryTaskSupplement: () => ({ mutate: vi.fn(), isPending: false }),
  useRequeueTaskSupplement: () => ({ mutate: vi.fn(), isPending: false }),
}));
vi.mock("@multica/core/api", () => ({ api: { previewCommentTriggers: vi.fn() } }));

const REQUEUE_LABEL = zhIssues.inline_run.supplement_requeue;

function entryWith(receipt: Record<string, unknown>): TimelineEntry {
  return {
    id: "comment-1",
    type: "comment",
    content: "这条没能送进去",
    supplements: [receipt],
  } as unknown as TimelineEntry;
}

function renderReceipts(entry: TimelineEntry) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <I18nProvider resources={TEST_RESOURCES} locale="zh-Hans">
        {/* SteerReceipts 是这个文件对外导出的入口 */}
        <SteerReceipts issueId="issue-1" entry={entry} />
      </I18nProvider>
    </QueryClientProvider>,
  );
}

describe("改为排到下一轮 (#42)", () => {
  // 每个用例渲染一份 DOM，不清掉的话「不应该出现」的断言会看到上一个用例残留的按钮，
  // 于是本该红的变成绿的——这类测试最危险的假绿就在这种地方。
  afterEach(cleanup);

  it("从未投递的失败回执给退路", () => {
    renderReceipts(entryWith({
      task_id: "task-1", agent_id: "agent-1", status: "failed",
      failure_reason: "turn_ended", attempt_count: 0,
    }));
    expect(screen.getByRole("button", { name: REQUEUE_LABEL })).toBeTruthy();
  });

  it("送出去过的失败回执不给退路", () => {
    renderReceipts(entryWith({
      task_id: "task-1", agent_id: "agent-1", status: "failed",
      failure_reason: "turn_ended", attempt_count: 1,
    }));
    expect(screen.queryByRole("button", { name: REQUEUE_LABEL })).toBeNull();
  });

  it("老服务端不带 attempt_count 时当不知道，不按 0 处理", () => {
    renderReceipts(entryWith({
      task_id: "task-1", agent_id: "agent-1", status: "failed",
      failure_reason: "turn_ended",
    }));
    expect(screen.queryByRole("button", { name: REQUEUE_LABEL })).toBeNull();
  });

  it("已送达的回执没有失败出口", () => {
    renderReceipts(entryWith({
      task_id: "task-1", agent_id: "agent-1", status: "delivered", attempt_count: 1,
    }));
    expect(screen.queryByRole("button", { name: REQUEUE_LABEL })).toBeNull();
  });

  it("按钮文案是简体中文，不是回落到英文", () => {
    expect(REQUEUE_LABEL).toBe("改为排到下一轮");
  });
});
