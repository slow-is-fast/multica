// @vitest-environment jsdom

// #48 的界面这一半，最容易做错的三件事都不靠肉眼能看出来：
//
//   1. 队列为零时**还要**显示「待审 0 条」。静默最常见的形态是「没有问题的时候什么
//      都不显示」，那时人分不清「都审完了」和「这个功能不见了」。
//   2. 「还不知道」不能说成 0。读失败时报 0 就是在说「都审完了」——而这个功能的
//      全部意义就是让人相信那件事没发生。
//   3. 失败提示必须是中文。服务端挂的是机器可读的码，界面的职责是翻译它，而不是把
//      服务端那句英文抛给用户。这条只有把错误码喂进去才验得到。
//
// 缓存用真 QueryClient + 预置数据，而不是把 react-query 整个 mock 掉：审批成功后
// 「这条从队列里消失」正是靠缓存更新实现的，mock 掉就只剩被测代码之外的空壳。

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ApiError } from "@multica/core/api/client";
import { ruelKnowledgeKeys } from "@multica/core/ruel/queries";
import type { RuelKnowledgeEntry } from "@multica/core/ruel/knowledge";
import zhRuel from "../locales/zh-Hans/ruel.json";
import { renderWithI18n } from "../test/i18n";
import { KnowledgeApprovalTab } from "./knowledge-approval";

const listPending = vi.hoisted(() => vi.fn());
const approveKnowledge = vi.hoisted(() => vi.fn());
const rejectKnowledge = vi.hoisted(() => vi.fn());
const toastError = vi.hoisted(() => vi.fn());
const toastSuccess = vi.hoisted(() => vi.fn());

vi.mock("sonner", () => ({ toast: { error: toastError, success: toastSuccess } }));

// api 换成三个桩，其余（errorCode / ApiError 这些纯函数与类）保持真的——失败提示的
// 翻译正是拿真 errorCode 去读真 ApiError，桩掉它就是在验桩。
vi.mock("@multica/core/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/api")>()),
  api: {
    listRuelPendingKnowledge: listPending,
    approveRuelKnowledge: approveKnowledge,
    rejectRuelKnowledge: rejectKnowledge,
  },
}));

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({ issueDetail: (id: string) => `/acme/issues/${id}` }),
  useCurrentWorkspace: () => ({ id: "ws-1", name: "Acme", avatar_url: null }),
}));
// AppLink 平时要求一个 NavigationProvider（web 与 desktop 各一套）。这里只关心它
// 渲染出的 href，所以换成裸 <a>——桩掉导航而不是桩掉整个组件。
vi.mock("../navigation", () => ({
  AppLink: ({
    href,
    children,
    className,
  }: {
    href: string;
    children: React.ReactNode;
    className?: string;
  }) => (
    <a href={href} className={className}>
      {children}
    </a>
  ),
}));

const KEY = ruelKnowledgeKeys.pending("ws-1");

function entry(overrides: Partial<RuelKnowledgeEntry> & { id: string }): RuelKnowledgeEntry {
  return {
    statement: "",
    rationale: "",
    status: "pending",
    source_task_id: "",
    source_issue_id: "",
    source_comment_id: "",
    review_note: "",
    reviewed_by_type: "",
    reviewed_by_id: "",
    reviewed_at: "",
    created_at: "2026-10-10T00:00:00Z",
    updated_at: "2026-10-10T00:00:00Z",
    ...overrides,
  };
}

/** 预置队列并渲染。staleTime 让预置数据被当成新鲜的，所以挂载时不会再打一次接口。 */
function renderWithQueue(rows: RuelKnowledgeEntry[] | undefined) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  if (rows) client.setQueryData(KEY, rows);
  renderWithI18n(
    <QueryClientProvider client={client}>
      <KnowledgeApprovalTab />
    </QueryClientProvider>,
    { locale: "zh-Hans" },
  );
  return client;
}

const approve = zhRuel.knowledge.approve;
const rejectLabel = zhRuel.knowledge.reject;
const rejectConfirm = zhRuel.knowledge.reject_confirm;

describe("项目知识待审队列 (#48)", () => {
  beforeEach(() => {
    // 每个用例之间清一次：不清的话「不该出现」的断言会看到上一个用例的 DOM，
    // 本该红的变成绿的——这类测试最危险的假绿就在这种地方。
    cleanup();
    listPending.mockReset().mockResolvedValue([]);
    approveKnowledge.mockReset();
    rejectKnowledge.mockReset();
    toastError.mockReset();
    toastSuccess.mockReset();
  });
  afterEach(cleanup);

  it("把候选、理由与来源摆出来，并报出待审计数", () => {
    renderWithQueue([
      entry({ id: "a", statement: "结论 A", rationale: "因为 A 的实测数据" }),
      entry({ id: "b", statement: "结论 B", source_issue_id: "issue-9" }),
    ]);

    expect(screen.getByText("结论 A")).toBeTruthy();
    expect(screen.getByText("因为 A 的实测数据")).toBeTruthy();
    expect(screen.getByText("结论 B")).toBeTruthy();
    expect(screen.getByText("待审 2 条")).toBeTruthy();

    // 来源是一条能点进去的链接：判断这条候选值不值得留，得先能去读它从哪来。
    const link = screen.getByRole("link", { name: zhRuel.knowledge.source_issue });
    expect(link.getAttribute("href")).toBe("/acme/issues/issue-9");
  });

  it("没有来源时明说「来源不详」，不留空白", () => {
    renderWithQueue([entry({ id: "a", statement: "来源丢了的结论" })]);
    expect(screen.getByText(zhRuel.knowledge.source_unknown)).toBeTruthy();
  });

  it("队列为空时仍然显示「待审 0 条」", () => {
    renderWithQueue([]);

    expect(screen.getByText(zhRuel.knowledge.empty_title)).toBeTruthy();
    // 这一行是本用例的重点：空队列与「功能不见了」必须长得不一样。
    expect(screen.getByText("待审 0 条")).toBeTruthy();
    expect(screen.queryByRole("button", { name: approve })).toBeNull();
  });

  it("读失败时不报 0，并给出中文说明与重试", async () => {
    listPending.mockRejectedValue(new Error("network down"));
    renderWithQueue(undefined);

    expect(await screen.findByText(zhRuel.knowledge.load_failed_title)).toBeTruthy();
    // 读不到就报 0，等于把「不知道」说成「都审完了」。
    expect(screen.queryByText(/待审 \d+ 条/)).toBeNull();
    expect(screen.getByRole("button", { name: zhRuel.knowledge.retry })).toBeTruthy();
  });

  it("批准：调接口、该行立刻从队列里消失、提示是中文", async () => {
    renderWithQueue([entry({ id: "a", statement: "结论 A" })]);
    // 重新拉取永不返回：这样「行消失」只可能来自审批成功后的缓存更新，
    // 而不是碰巧赶上了一次刷新。
    listPending.mockReturnValue(new Promise(() => {}));
    approveKnowledge.mockResolvedValue(
      entry({ id: "a", statement: "结论 A", status: "approved", reviewed_by_id: "u-1" }),
    );

    fireEvent.click(screen.getByRole("button", { name: approve }));

    await waitFor(() => expect(approveKnowledge).toHaveBeenCalledWith("a"));
    await waitFor(() => expect(screen.queryByText("结论 A")).toBeNull());
    expect(toastSuccess).toHaveBeenCalledWith(zhRuel.knowledge.approved_toast);
    expect(toastError).not.toHaveBeenCalled();
  });

  it("响应解析不出来时不动界面，等队列重新拉取", async () => {
    renderWithQueue([entry({ id: "a", statement: "结论 A" })]);
    listPending.mockReturnValue(new Promise(() => {}));
    // 客户端在解析失败时返回 undefined——这时不能拿一条空记录去更新缓存。
    approveKnowledge.mockResolvedValue(undefined);

    fireEvent.click(screen.getByRole("button", { name: approve }));

    await waitFor(() => expect(approveKnowledge).toHaveBeenCalledWith("a"));
    expect(toastSuccess).toHaveBeenCalledWith(zhRuel.knowledge.approved_toast);
    // 行还在：没有可信的条目可用，界面宁可留着它，也不猜哪一条该消失。
    expect(screen.getByText("结论 A")).toBeTruthy();
  });

  it("拒绝：空理由不许提交，提交时带上去掉首尾空白的理由", async () => {
    renderWithQueue([entry({ id: "a", statement: "结论 A" })]);
    listPending.mockReturnValue(new Promise(() => {}));
    rejectKnowledge.mockResolvedValue(
      entry({ id: "a", statement: "结论 A", status: "rejected", review_note: "已被新方案替代" }),
    );

    fireEvent.click(screen.getByRole("button", { name: rejectLabel }));
    const confirm = screen.getByRole("button", {
      name: rejectConfirm,
    }) as HTMLButtonElement;
    // 理由必填不是后端的规矩，是界面上先挡住的一道：留痕 = 记人 + 记时间 + 记为什么。
    expect(confirm.disabled).toBe(true);

    // 只有空白也不算写了理由。
    const box = screen.getByLabelText(zhRuel.knowledge.reject_reason_label);
    fireEvent.change(box, { target: { value: "   \n  " } });
    expect(confirm.disabled).toBe(true);

    fireEvent.change(box, { target: { value: "  已被新方案替代  " } });
    expect(confirm.disabled).toBe(false);
    fireEvent.click(confirm);

    await waitFor(() => expect(rejectKnowledge).toHaveBeenCalledWith("a", "已被新方案替代"));
    await waitFor(() => expect(screen.queryByText("结论 A")).toBeNull());
    expect(toastSuccess).toHaveBeenCalledWith(zhRuel.knowledge.rejected_toast);
  });

  it("拒绝可以取消，取消后理由不残留", async () => {
    renderWithQueue([entry({ id: "a", statement: "结论 A" })]);

    fireEvent.click(screen.getByRole("button", { name: rejectLabel }));
    fireEvent.change(screen.getByLabelText(zhRuel.knowledge.reject_reason_label), {
      target: { value: "写了一半" },
    });
    fireEvent.click(screen.getByRole("button", { name: zhRuel.knowledge.reject_cancel }));

    expect(screen.queryByLabelText(zhRuel.knowledge.reject_reason_label)).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: rejectLabel }));
    const box = screen.getByLabelText(zhRuel.knowledge.reject_reason_label) as HTMLTextAreaElement;
    expect(box.value).toBe("");
  });

  it("409 的提示是中文，不是服务端那句英文", async () => {
    renderWithQueue([entry({ id: "a", statement: "结论 A" })]);
    listPending.mockReturnValue(new Promise(() => {}));
    rejectKnowledge.mockRejectedValue(
      new ApiError(
        "knowledge: 条目当前是 \"approved\"，只有待审条目能被批准或拒绝",
        409,
        "Conflict",
        { code: "knowledge_already_reviewed", error: "knowledge: ..." },
      ),
    );

    fireEvent.click(screen.getByRole("button", { name: rejectLabel }));
    fireEvent.change(screen.getByLabelText(zhRuel.knowledge.reject_reason_label), {
      target: { value: "反悔" },
    });
    fireEvent.click(screen.getByRole("button", { name: rejectConfirm }));

    await waitFor(() => expect(toastError).toHaveBeenCalledWith(zhRuel.knowledge.failed_already_reviewed));
    // 中文文案本身也要钉住：落到英文占位上这条断言会红。
    expect(zhRuel.knowledge.failed_already_reviewed).toBe("这条已经被处理过了，队列已刷新");
  });

  it("认不出的错误码退回中文泛化提示，不泄露服务端措辞", async () => {
    renderWithQueue([entry({ id: "a", statement: "结论 A" })]);
    listPending.mockReturnValue(new Promise(() => {}));
    approveKnowledge.mockRejectedValue(
      new ApiError("failed to review knowledge", 500, "Internal Server Error", {
        error: "failed to review knowledge",
      }),
    );

    fireEvent.click(screen.getByRole("button", { name: approve }));

    await waitFor(() => expect(toastError).toHaveBeenCalledWith(zhRuel.knowledge.failed_generic));
    expect(zhRuel.knowledge.failed_generic).toBe("操作没能完成，请重试");
  });
});
