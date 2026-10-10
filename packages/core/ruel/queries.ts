// Ruel 新增：产物查询的 cache key 与 queryOptions。
//
// 单独一个文件而不是并进 issues/queries.ts，理由与 artifacts.ts 相同——将来同步
// 上游时这个目录不与上游文件打架。

import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";
import type { RuelArtifact } from "./artifacts";
import type { RuelKnowledgeEntry } from "./knowledge";

export const ruelArtifactKeys = {
  all: () => ["ruel", "artifacts"] as const,
  /** 某个 Issue 下所有 Run 的产物。 */
  issue: (issueId: string) => [...ruelArtifactKeys.all(), issueId] as const,
};

/**
 * 一个 Issue 下全部 Run 的产物。
 *
 * Run 详情页（transcript）与 Issue 侧栏共用这一份数据：前者按 task_id 过滤出本轮，
 * 后者用全量统计累计变更。两个维度、一次请求，避免 N 个 Run 各打一次接口。
 *
 * staleTime 与 issueTasksOptions 对齐（30s）：产物是在 Run 终态前采集的，Run 一结束
 * 就不再变化；窗口重新聚焦时刷新，是为了让「刚跑完的那一轮」能自己出现。
 */
export function issueArtifactsOptions(issueId: string) {
  return queryOptions({
    queryKey: ruelArtifactKeys.issue(issueId),
    queryFn: () => api.listRuelIssueArtifacts(issueId),
    enabled: !!issueId,
    staleTime: 30_000,
    refetchOnWindowFocus: true,
  });
}

export type { RuelArtifact };

/**
 * 项目知识候选的 cache key（#48）。
 *
 * 带 workspace id：知识是 workspace 的硬边界（PRD 6.7），缓存也必须按它切。
 * 不带的话，切项目之后界面会先把上一个项目的待审队列摆出来，而队列里的每条都
 * 点得动——一个跨项目的误批准只需要一次切换加一次点击。
 */
export const ruelKnowledgeKeys = {
  all: (wsId: string) => ["ruel", "knowledge", wsId] as const,
  /** 某个 workspace 的待审队列。 */
  pending: (wsId: string) => [...ruelKnowledgeKeys.all(wsId), "pending"] as const,
};

/**
 * 待审队列。
 *
 * staleTime 与人怎么用这个界面有关：队列是人点出来的，不是流式界面——没有轮询，
 * 窗口重新聚焦时取一次。审批成功之后由 mutations 主动作废这个 key，所以「刚批完
 * 的那条还在」不会停留到下一次聚焦。
 */
export function pendingKnowledgeOptions(wsId: string) {
  return queryOptions({
    queryKey: ruelKnowledgeKeys.pending(wsId),
    queryFn: () => api.listRuelPendingKnowledge(),
    enabled: !!wsId,
    staleTime: 30_000,
    refetchOnWindowFocus: true,
  });
}

export type { RuelKnowledgeEntry };
