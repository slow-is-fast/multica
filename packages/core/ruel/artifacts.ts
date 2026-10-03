// Ruel 新增：Run 产物的传输层。
//
// 上游把一次 Run 的结果塞进 agent_task_queue.result，只有 output / pr_url /
// work_dir / session_id 四项——没有 diff，也没有测试证据。产物的采集在
// server/internal/daemon/ruel_artifacts.go，落点在 server/internal/ruel/artifacts，
// 这一层只负责把它搬到前端。
//
// 刻意放进 `ruel/` 命名空间而不并进 api/schemas.ts：将来同步上游时这个目录不会与
// 上游任何文件打架，一眼能看出哪些是我们自己加的。
//
// 与服务端同一个取舍：产物是「附加证据」，不是主流程状态。所以它缺了、多了、
// 字段变了，都不该让界面上「这次 Run 改了什么」整块消失——每个字段独立降级。

import { z } from "zod";

/** 采集侧目前写出的四种产物。test_log 留作后续接 CI 输出的坑位。 */
export const RUEL_ARTIFACT_KINDS = [
  "diff",
  "diff_stat",
  "file_change",
  "test_log",
] as const;

export type RuelArtifactKind = (typeof RUEL_ARTIFACT_KINDS)[number];

/**
 * 采集结论的 kind。它不是产物——产物回答「这轮改了什么」，结论回答「这轮为什么没有
 * 产物」。刻意不并进 RUEL_ARTIFACT_KINDS：那是产物种类的白名单，混进去会让
 * artifactOfKind 之类的调用把结论也当成一种产物取出来。
 */
export const RUEL_COLLECTION_STATUS_KIND = "collection_status";

/**
 * 一次采集的结论。
 *
 * 四种取值对应「产物表为空」的四种含义，缺了这一层它们长得一模一样：
 *
 * - `changed`：采到了。这一轮有产物，不需要解释。
 * - `no_repo`：这轮没碰代码仓库（agent 只在容器目录里活动）。正常。
 * - `no_change`：碰了仓库但确实没改动（只读任务 / 改动被 revert）。正常。
 * - `collect_failed`：git 缺失、超时、权限问题。**这是缺陷**，不是「没干活」。
 *
 * 另外还有一种界面状态叫「压根没采集」——连结论都没有，说明 daemon 没上报成功。
 * 它由结论缺失来表达，不单列一个值。
 */
export const RUEL_COLLECTION_STATUSES = [
  "changed",
  "no_repo",
  "no_change",
  "collect_failed",
] as const;

export type RuelCollectionStatus = (typeof RUEL_COLLECTION_STATUSES)[number];

function isRuelCollectionStatus(value: string): value is RuelCollectionStatus {
  return (RUEL_COLLECTION_STATUSES as readonly string[]).includes(value);
}

/** 这一行是不是采集结论（而不是产物）。 */
export function isCollectionStatus(artifact: RuelArtifact): boolean {
  return artifact.kind === RUEL_COLLECTION_STATUS_KIND;
}

/**
 * 取一次 Run 的采集结论。认不出的取值一律当「没有结论」——结论是解释性文本，
 * 认不出就退回「不知道」，也不能让一个拼错的词把界面卡住。
 */
export function collectionStatusOf(
  artifacts: readonly RuelArtifact[] | undefined,
): RuelCollectionStatus | undefined {
  const row = artifacts?.find(isCollectionStatus);
  if (!row) return undefined;
  return isRuelCollectionStatus(row.content) ? row.content : undefined;
}

/**
 * 把产物与结论分开。
 *
 * 服务端为省一次查询，把结论作为 kind=collection_status 的一行混在列表里返回；这里
 * 在客户端分流，调用方拿到的 items 就只会是真正的产物。
 */
export function splitRuelArtifacts(artifacts: readonly RuelArtifact[]): {
  items: RuelArtifact[];
  statusByTask: Map<string, RuelCollectionStatus>;
} {
  const items: RuelArtifact[] = [];
  const statusByTask = new Map<string, RuelCollectionStatus>();
  for (const artifact of artifacts) {
    if (isCollectionStatus(artifact)) {
      if (isRuelCollectionStatus(artifact.content)) {
        statusByTask.set(artifact.task_id, artifact.content);
      }
      continue;
    }
    items.push(artifact);
  }
  return { items, statusByTask };
}

export interface RuelArtifact {
  id: string;
  task_id: string;
  kind: string;
  uri: string;
  size: number;
  checksum: string;
  content: string;
  created_at: string;
}

export const RuelArtifactSchema = z.object({
  id: z.string().default(""),
  task_id: z.string().default(""),
  kind: z.string().default(""),
  uri: z.string().default(""),
  size: z.number().default(0),
  checksum: z.string().default(""),
  content: z.string().default(""),
  created_at: z.string().default(""),
});

export const RuelArtifactListSchema = z.array(RuelArtifactSchema);

/**
 * 按 Run 分组。Issue 页要的是「这个需求总共改了什么」，Run 页要的是「这一轮改了
 * 什么」，两者是同一份数据按不同维度切——所以只拉一次，在客户端分组，而不是每个
 * Run 各发一个请求。
 *
 * 服务端按 created_at 排序返回，分组内保持这个顺序。
 */
export function groupArtifactsByTask(
  artifacts: readonly RuelArtifact[],
): Map<string, RuelArtifact[]> {
  const out = new Map<string, RuelArtifact[]>();
  for (const artifact of artifacts) {
    const bucket = out.get(artifact.task_id);
    if (bucket) bucket.push(artifact);
    else out.set(artifact.task_id, [artifact]);
  }
  return out;
}

/** 取一次 Run 的某类产物。同一 Run 的同类产物在服务端是覆盖写入，只有一份。 */
export function artifactOfKind(
  artifacts: readonly RuelArtifact[] | undefined,
  kind: RuelArtifactKind,
): RuelArtifact | undefined {
  return artifacts?.find((artifact) => artifact.kind === kind);
}
