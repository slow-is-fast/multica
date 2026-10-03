// Ruel 新增：产物查询的 cache key 与 queryOptions。
//
// 单独一个文件而不是并进 issues/queries.ts，理由与 artifacts.ts 相同——将来同步
// 上游时这个目录不与上游文件打架。

import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";
import type { RuelArtifact } from "./artifacts";

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
