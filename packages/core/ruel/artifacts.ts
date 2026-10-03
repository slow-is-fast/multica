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
