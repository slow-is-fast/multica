// Ruel 新增：项目知识候选的传输层（#48）。
//
// 与 artifacts.ts 是同一层的邻居，形状刻意保持一致：zod schema + 纯函数，不碰网络。
// 服务端接口见 server/internal/handler/ruel_knowledge.go。
//
// 降级策略也照抄产物那一层：知识是附加上下文，字段多了少了都不该让整块界面消失，
// 所以每个字段各带默认值，而不是让一条记录解析失败就整页空白。
//
// 唯一与产物不同的是「没有」的表达方式。服务端用 null 表示「还没有人审」
// （reviewed_*）和「没有这个来源」（source_*），这里在解析时就把 null / undefined
// 一并归成空串。三种「没有」一路传到渲染层是渲染 bug 的常见出处：只写 truthy
// 判断会漏掉 null 之外的形态，而漏掉的那一种恰好只在某个分支上出现。

import { z } from "zod";

/**
 * 审阅状态。与迁移 565 的 CHECK 逐字对应。
 *
 * 这里不做白名单校验（schema 用 z.string()）：状态是服务端的事实，客户端认不出
 * 一个新状态时应当把它原样显示出来，而不是把整条记录判为非法——那会让一次服务端
 * 的新增状态变成界面上的一整块空白。
 */
export const RUEL_KNOWLEDGE_STATUSES = [
  "pending",
  "approved",
  "rejected",
  "archived",
] as const;

export type RuelKnowledgeStatus = (typeof RUEL_KNOWLEDGE_STATUSES)[number];

export interface RuelKnowledgeEntry {
  id: string;
  /** 结论本身。审批看的就是这句话。 */
  statement: string;
  /** 为什么值得记住。空的表示提案方没给。 */
  rationale: string;
  status: string;
  /** 溯源三件套。三个都可能为空：溯源指向的东西消失了也不该带走知识。 */
  source_task_id: string;
  source_issue_id: string;
  source_comment_id: string;
  /** 审阅意见。拒绝时必有；批准时可能为空。 */
  review_note: string;
  reviewed_by_type: string;
  reviewed_by_id: string;
  reviewed_at: string;
  created_at: string;
  updated_at: string;
}

export const RuelKnowledgeEntrySchema = z.object({
  id: z.string().default(""),
  statement: z.string().default(""),
  rationale: z.string().default(""),
  status: z.string().default(""),
  source_task_id: z
    .string()
    .nullish()
    .transform((v) => v ?? ""),
  source_issue_id: z
    .string()
    .nullish()
    .transform((v) => v ?? ""),
  source_comment_id: z
    .string()
    .nullish()
    .transform((v) => v ?? ""),
  review_note: z.string().default(""),
  reviewed_by_type: z
    .string()
    .nullish()
    .transform((v) => v ?? ""),
  reviewed_by_id: z
    .string()
    .nullish()
    .transform((v) => v ?? ""),
  reviewed_at: z
    .string()
    .nullish()
    .transform((v) => v ?? ""),
  created_at: z.string().default(""),
  updated_at: z.string().default(""),
});

export const RuelKnowledgeListSchema = z.array(RuelKnowledgeEntrySchema);

/**
 * 一条候选是从哪里总结出来的。
 *
 * 分三档而不是给一个通用 id：三种来源在界面上的去处完全不同——Issue 可以点进去
 * 读整条线索（评论与 Run 都在它的上下文里），评论与 Run 只能作为文字说明。
 *
 * 优先级 Issue > 评论 > Run：能点进去的那一档优先。三者都为空时返回 undefined，
 * 界面显示「来源不详」而不是一个空白的链接位——空白会让人以为界面坏了。
 */
export type RuelKnowledgeSource =
  | { kind: "issue"; id: string }
  | { kind: "comment"; id: string }
  | { kind: "task"; id: string };

export function knowledgeSourceOf(
  entry: RuelKnowledgeEntry,
): RuelKnowledgeSource | undefined {
  if (entry.source_issue_id) return { kind: "issue", id: entry.source_issue_id };
  if (entry.source_comment_id) return { kind: "comment", id: entry.source_comment_id };
  if (entry.source_task_id) return { kind: "task", id: entry.source_task_id };
  return undefined;
}

/**
 * 还有多少条等着人看。
 *
 * 单独一个函数而不是在组件里写 entries.length：待审计数要在三种状态下都给得出
 * （加载中、请求失败、正常的空队列），而「加载中」与「失败」都不能报 0——报 0 就是
 * 在说「都审完了」，那正是这个数要防的误报。数据没到就返回 undefined，让调用方
 * 自己决定显示什么。
 */
export function pendingKnowledgeCount(
  entries: readonly RuelKnowledgeEntry[] | undefined,
): number | undefined {
  return entries ? entries.length : undefined;
}
