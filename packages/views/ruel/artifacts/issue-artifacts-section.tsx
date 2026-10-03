"use client";

// Ruel 新增：Issue 侧栏里「这个需求累计改了什么」。
//
// 之所以要有这一块，是因为只看最后一轮会漏东西：agent 常见做法是先写一版、被指出问题
// 后 revert 再重做。最后一轮的 diff 里没有那些被推翻又被重写的部分，但那正是验收时
// 要问「你到底动过哪些文件」的答案。所以这里按 Run 逐轮列出，不做净额合并——合并
// 出来的净额恰好会把这些往返抹平，那正是要避免的。
//
// 顺序用时间正序而不是倒序：侧栏其它区块（执行日志）倒序是因为它们在回答「最近发生
// 了什么」，这里回答的是「这个需求怎么一路改过来的」，倒着读不出来。

import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { ChevronRight, FileDiff } from "lucide-react";
import { issueArtifactsOptions } from "@multica/core/ruel/queries";
import {
  artifactOfKind,
  groupArtifactsByTask,
  splitRuelArtifacts,
  type RuelArtifact,
  type RuelCollectionStatus,
} from "@multica/core/ruel/artifacts";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";
import { EmptyReason } from "./collection-status";
import {
  countDiffLines,
  parseDiffStat,
  parseFileChanges,
  parseUnifiedDiff,
  type FileChange,
} from "./parse";

interface RunChanges {
  taskId: string;
  /** 第几轮，从 1 开始，按产物产生的时间正序编号。 */
  index: number;
  /** 这一轮的采集结论。没有产物的轮次靠它说明原因，而不是留一片空白。 */
  status: RuelCollectionStatus | undefined;
  changes: FileChange[];
  added: number;
  removed: number;
}

export function IssueArtifactsSection({ issueId }: { issueId: string }) {
  const { t } = useT("ruel");
  const [open, setOpen] = useState(true);
  const { data: artifacts = [] } = useQuery(issueArtifactsOptions(issueId));

  const runs = useMemo(() => buildRuns(artifacts), [artifacts]);

  // 一个产物都没有就整块不渲染。侧栏已经够挤了，没有内容可展示的区块不该占位。
  if (runs.length === 0) return null;

  // 累计口径是「去重后动过多少文件」：同一文件在多轮里被反复改，算一个文件，但每一轮
  // 各自的改动仍然逐条列在下面。
  const touched = new Set<string>();
  for (const run of runs) for (const change of run.changes) touched.add(change.path);
  const added = runs.reduce((sum, run) => sum + run.added, 0);
  const removed = runs.reduce((sum, run) => sum + run.removed, 0);

  return (
    <div>
      <div className="mb-2 flex w-full items-center gap-1">
        <button
          type="button"
          className={cn(
            "flex min-w-0 items-center gap-1 whitespace-nowrap rounded-md px-2 py-1 text-caption font-medium transition-colors hover:bg-accent/70",
            open ? "" : "text-muted-foreground hover:text-foreground",
          )}
          onClick={() => setOpen(!open)}
          aria-expanded={open}
        >
          <FileDiff className="size-3 shrink-0 text-muted-foreground" />
          <span className="truncate">{t(($) => $.artifacts.section_title)}</span>
          {touched.size > 0 && (
            <span className="rounded-xs bg-muted px-1 text-micro font-medium tabular-nums text-muted-foreground">
              {t(($) => $.artifacts.files, { count: touched.size })}
            </span>
          )}
          <ChevronRight
            className={cn(
              "!size-3 shrink-0 stroke-[2.5] text-muted-foreground transition-transform",
              open && "rotate-90",
            )}
          />
        </button>
        {(added > 0 || removed > 0) && (
          <span className="ml-auto shrink-0 font-mono text-caption tabular-nums">
            {added > 0 && <span className="text-success">+{added}</span>}
            {removed > 0 && <span className="text-destructive">-{removed}</span>}
          </span>
        )}
      </div>

      {open && (
        <div className="space-y-1 pl-2">
          {runs.map((run) => (
            <RunRow key={run.taskId} run={run} />
          ))}
        </div>
      )}
    </div>
  );
}

function RunRow({ run }: { run: RunChanges }) {
  const { t } = useT("ruel");
  const [expanded, setExpanded] = useState(false);

  // 没有改动的轮次不做成可展开的行：点开只会看到一句「没有变更」，那句话直接写在行里
  // 更好——少一次点击，也少一个空区块。
  if (run.changes.length === 0) {
    return (
      <div className="flex w-full items-center gap-1.5 px-1 py-1 text-caption">
        <span className="shrink-0 text-micro text-muted-foreground">
          {t(($) => $.artifacts.round, { index: run.index })}
        </span>
        <EmptyReason status={run.status} className="truncate text-micro" />
      </div>
    );
  }

  return (
    <div>
      <button
        type="button"
        onClick={() => setExpanded(!expanded)}
        aria-expanded={expanded}
        className="flex w-full items-center gap-1.5 rounded-xs px-1 py-1 text-left text-caption transition-colors hover:bg-accent/40"
      >
        <span className="shrink-0 text-micro text-muted-foreground">
          {t(($) => $.artifacts.round, { index: run.index })}
        </span>
        <span className="shrink-0 text-micro text-muted-foreground">
          {t(($) => $.artifacts.files, { count: run.changes.length })}
        </span>
        <span className="ml-auto shrink-0 font-mono text-micro tabular-nums">
          {run.added > 0 && <span className="text-success">+{run.added}</span>}
          {run.removed > 0 && <span className="text-destructive">-{run.removed}</span>}
        </span>
      </button>
      {expanded && (
        <ul className="space-y-0.5 pb-1 pl-3">
          {run.changes.map((change) => (
            <li
              key={`${change.kind}:${change.path}`}
              className="truncate font-mono text-micro text-muted-foreground"
              title={change.fromPath ? `${change.fromPath} → ${change.path}` : change.path}
            >
              {change.path}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/**
 * 把产物按 Run 分组并编号。
 *
 * 服务端按 created_at 排序返回，所以这里按首次出现的顺序编号就是时间正序。
 *
 * 顺序取自**未分流**的列表：只有结论没有产物的那一轮（只读、没检出仓库、采集失败）在
 * items 里不留痕迹，只看 items 就会把它漏掉，而它同样占一个轮次编号。
 */
function buildRuns(artifacts: readonly RuelArtifact[]): RunChanges[] {
  const { items, statusByTask } = splitRuelArtifacts(artifacts);
  const grouped = groupArtifactsByTask(items);

  const ordered: string[] = [];
  const seen = new Set<string>();
  for (const artifact of artifacts) {
    if (seen.has(artifact.task_id)) continue;
    seen.add(artifact.task_id);
    ordered.push(artifact.task_id);
  }

  const runs: RunChanges[] = [];
  let index = 0;
  for (const taskId of ordered) {
    const list = grouped.get(taskId) ?? [];
    index += 1;
    runs.push({
      taskId,
      index,
      status: statusByTask.get(taskId),
      changes: parseFileChanges(artifactOfKind(list, "file_change")?.content ?? ""),
      // 有 --stat 就用它算：它是纯文本表格，比把整份 diff 解析一遍便宜得多。没有
      // （比如采集时 git 没输出）才退回解析 diff 正文。
      ...(lineCountsOf(list) ?? { added: 0, removed: 0 }),
    });
  }
  return runs;
}

function lineCountsOf(
  list: readonly RuelArtifact[],
): { added: number; removed: number } | null {
  const stat = parseDiffStat(artifactOfKind(list, "diff_stat")?.content ?? "");
  if (stat.length > 0) {
    let added = 0;
    let removed = 0;
    for (const entry of stat) {
      added += entry.added;
      removed += entry.removed;
    }
    return { added, removed };
  }
  const diff = artifactOfKind(list, "diff")?.content;
  if (!diff) return null;
  return countDiffLines(parseUnifiedDiff(diff));
}
