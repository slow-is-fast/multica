// Ruel 新增：#48 传输层的纯函数与降级策略。
//
// 这些用例不渲染任何东西，因为要验的事情发生在渲染之前：服务端把「没有」写成 null，
// 而界面只认空串。这一层若漏了 null，失败不是「显示得难看」，是整条记录在界面上
// 一半有值一半没有——而那一半恰好是最容易被忽略的分支（待审条目的 reviewed_* 全是
// null，也就是最常见的那一条）。

import { describe, expect, it } from "vitest";
import {
  knowledgeSourceOf,
  pendingKnowledgeCount,
  RuelKnowledgeEntrySchema,
  RuelKnowledgeListSchema,
  type RuelKnowledgeEntry,
} from "./knowledge";

/** 服务端返回的那一行：待审时 reviewed_* 与 source_* 都是 null。 */
const WIRE_PENDING = {
  id: "k-1",
  statement: "不要用全局变量传状态",
  rationale: "这一轮踩过一次",
  status: "pending",
  source_task_id: null,
  source_issue_id: null,
  source_comment_id: null,
  review_note: "",
  reviewed_by_type: null,
  reviewed_by_id: null,
  reviewed_at: null,
  created_at: "2026-10-10T00:00:00Z",
  updated_at: "2026-10-10T00:00:00Z",
};

describe("项目知识条目的解析 (#48)", () => {
  it("把服务端的 null 归成空串，而不是把它留给渲染层", () => {
    const parsed = RuelKnowledgeEntrySchema.parse(WIRE_PENDING) as RuelKnowledgeEntry;
    expect(parsed.source_issue_id).toBe("");
    expect(parsed.reviewed_by_id).toBe("");
    expect(parsed.reviewed_at).toBe("");
    // 有值的那几个字段不能顺带被抹掉。
    expect(parsed.statement).toBe("不要用全局变量传状态");
    expect(parsed.status).toBe("pending");
  });

  it("字段缺失也算「没有」，不是解析失败", () => {
    // 老服务端不发这些字段。整条记录不能因此被丢掉——知识是附加上下文，
    // 缺席的字段应当降级为空，而不是让审批页整块消失。
    const parsed = RuelKnowledgeEntrySchema.parse({ id: "k-2" }) as RuelKnowledgeEntry;
    expect(parsed.id).toBe("k-2");
    expect(parsed.statement).toBe("");
    expect(parsed.reviewed_at).toBe("");
  });

  it("列表解析：空数组仍然是空数组", () => {
    expect(RuelKnowledgeListSchema.parse([])).toEqual([]);
  });

  it("认不出的状态原样留下，不判非法", () => {
    // 状态是服务端的事实。客户端认不出一个新状态时应当把它显示出来，
    // 而不是把整条记录判为非法——那会让一次服务端新增状态变成一整块空白。
    const parsed = RuelKnowledgeEntrySchema.parse({
      ...WIRE_PENDING,
      status: "quarantined",
    }) as RuelKnowledgeEntry;
    expect(parsed.status).toBe("quarantined");
  });
});

describe("来源的判定 (#48)", () => {
  const base: RuelKnowledgeEntry = {
    id: "k-1",
    statement: "s",
    rationale: "",
    status: "pending",
    source_task_id: "",
    source_issue_id: "",
    source_comment_id: "",
    review_note: "",
    reviewed_by_type: "",
    reviewed_by_id: "",
    reviewed_at: "",
    created_at: "",
    updated_at: "",
  };

  it("能点进去的那一档优先：Issue 赢过评论与 Run", () => {
    expect(
      knowledgeSourceOf({
        ...base,
        source_issue_id: "issue-1",
        source_comment_id: "comment-1",
        source_task_id: "task-1",
      }),
    ).toEqual({ kind: "issue", id: "issue-1" });
  });

  it("没有 Issue 时退到评论，再退到 Run", () => {
    expect(
      knowledgeSourceOf({ ...base, source_comment_id: "comment-1", source_task_id: "task-1" }),
    ).toEqual({ kind: "comment", id: "comment-1" });
    expect(knowledgeSourceOf({ ...base, source_task_id: "task-1" })).toEqual({
      kind: "task",
      id: "task-1",
    });
  });

  it("三个都为空时返回 undefined，由界面明说「来源不详」", () => {
    expect(knowledgeSourceOf(base)).toBeUndefined();
  });
});

describe("待审计数 (#48)", () => {
  it("数据没到是 undefined，不是 0", () => {
    // 这一条是整个计数唯一要紧的地方：0 代表「都审完了」，
    // 而「还不知道」不能借用这句话。
    expect(pendingKnowledgeCount(undefined)).toBeUndefined();
  });

  it("空队列是 0，不是 undefined", () => {
    expect(pendingKnowledgeCount([])).toBe(0);
  });

  it("有候选时就是条数", () => {
    const row = { id: "a" } as RuelKnowledgeEntry;
    expect(pendingKnowledgeCount([row, row])).toBe(2);
  });
});
