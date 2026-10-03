"use client";

// Ruel 新增：一次 Run 的变更（P0-6 要求的 diff 与文件变更列表）。
//
// 挂载在 transcript 里——上游把一次 Run 的全部细节（结果、Agent、耗时、花费、逐步
// 时间线）都放在那一个弹层里，它是事实上的「Run 详情页」。产物不另起页面，是因为
// #16 已经拍板 UI 复用上游，再开一套页面等于把那条决定推翻。
//
// 渲染面直接复用上游的 DiffDetailSurface：语法高亮、+/- 计数、超长折叠那套行为都
// 是现成的，产物只是另一份数据来源，没必要再写一遍 diff 渲染。

import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { ChevronRight, FileDiff } from "lucide-react";
import { issueArtifactsOptions } from "@multica/core/ruel/queries";
import {
  artifactOfKind,
  collectionStatusOf,
  isCollectionStatus,
} from "@multica/core/ruel/artifacts";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";
import { DiffDetailSurface } from "../../common/task-transcript/detail-surfaces";
import { EmptyReason } from "./collection-status";
import {
  countDiffLines,
  isTruncated,
  parseDiffStat,
  parseFileChanges,
  parseUnifiedDiff,
  type DiffFile,
  type DiffStatEntry,
  type FileChange,
} from "./parse";

export interface RunArtifactsPanelProps {
  issueId: string;
  /** 本次 Run 的 task id。产物按 Issue 拉取一次，在这里按 Run 过滤。 */
  taskId: string;
  /** 默认展开。Run 详情页里默认收起，避免一打开就被 diff 占满。 */
  defaultOpen?: boolean;
}

/**
 * 一次 Run 改了什么。
 *
 * 没有产物时不再一律整块不渲染，而是先看有没有采集结论：有结论就显示「这一轮为什么
 * 没有产物」（没检出仓库 / 没改动 / 采集失败），连结论都没有才整块不渲染——那种情况
 * 意味着这轮压根没采集过，给它一个「变更：无」的区块反而是在把未知伪装成已知。
 */
export function RunArtifactsPanel({
  issueId,
  taskId,
  defaultOpen = false,
}: RunArtifactsPanelProps) {
  const { t } = useT("ruel");
  const [open, setOpen] = useState(defaultOpen);
  const { data: artifacts = [] } = useQuery(issueArtifactsOptions(issueId));

  const mine = useMemo(
    () => artifacts.filter((artifact) => artifact.task_id === taskId),
    [artifacts, taskId],
  );
  // 结论行（kind=collection_status）不是产物，分流出去，否则「有产物」的判断会被它
  // 带偏：一个只有结论行的 Run 会被当成有产物，展开后是一块空的变更列表。
  const items = useMemo(() => mine.filter((artifact) => !isCollectionStatus(artifact)), [mine]);
  const status = useMemo(() => collectionStatusOf(mine), [mine]);

  const changes = useMemo(
    () => parseFileChanges(artifactOfKind(items, "file_change")?.content ?? ""),
    [items],
  );
  const statEntries = useMemo(
    () => parseDiffStat(artifactOfKind(items, "diff_stat")?.content ?? ""),
    [items],
  );
  const diffFiles = useMemo(
    () => parseUnifiedDiff(artifactOfKind(items, "diff")?.content ?? ""),
    [items],
  );
  const truncated = useMemo(
    () => isTruncated(artifactOfKind(items, "diff")?.content ?? ""),
    [items],
  );

  const { added, removed } = countDiffLines(diffFiles);
  const checksum = artifactOfKind(items, "diff")?.checksum ?? "";

  // 没有产物但有结论：显示这一轮为什么没有产物。没有结论才整块不渲染。
  if (items.length === 0) {
    if (!status) return null;
    return (
      <div className="shrink-0 border-b px-4 py-2">
        <div className="flex w-full items-center gap-1.5 px-1 py-1 text-caption">
          <FileDiff className="size-3.5 shrink-0 text-muted-foreground" />
          <span className="truncate">{t(($) => $.artifacts.run_title)}</span>
          <EmptyReason status={status} className="ml-auto shrink-0 truncate text-micro" />
        </div>
      </div>
    );
  }

  return (
    <div className="shrink-0 border-b px-4 py-2">
      <button
        type="button"
        onClick={() => setOpen(!open)}
        aria-expanded={open}
        aria-label={t(($) => $.artifacts.toggle_aria)}
        className={cn(
          "flex w-full items-center gap-1.5 rounded-md px-1 py-1 text-caption font-medium transition-colors hover:bg-accent/70",
          open ? "" : "text-muted-foreground hover:text-foreground",
        )}
      >
        <FileDiff className="size-3.5 shrink-0 text-muted-foreground" />
        <span className="truncate">{t(($) => $.artifacts.run_title)}</span>
        {changes.length > 0 && (
          <span className="rounded-xs bg-muted px-1 text-micro font-medium tabular-nums text-muted-foreground">
            {t(($) => $.artifacts.files, { count: changes.length })}
          </span>
        )}
        {added > 0 && (
          <span className="shrink-0 font-mono text-micro tabular-nums text-success">
            +{added}
          </span>
        )}
        {removed > 0 && (
          <span className="shrink-0 font-mono text-micro tabular-nums text-destructive">
            -{removed}
          </span>
        )}
        {checksum && (
          <span
            className="ml-auto max-w-24 shrink-0 truncate font-mono text-micro text-faint-foreground"
            title={t(($) => $.artifacts.checksum_title, { sum: checksum })}
          >
            {checksum.slice(0, 8)}
          </span>
        )}
        {/* 校验值存在时由它吃掉剩余空间，不存在时由箭头吃掉——两个都写 ml-auto 的话
            剩余空间会被它们平分，箭头就不贴右边了。 */}
        <ChevronRight
          className={cn(
            "size-3 shrink-0 stroke-[2.5] text-muted-foreground transition-transform",
            checksum ? "" : "ml-auto",
            open && "rotate-90",
          )}
        />
      </button>

      {open && (
        // 弹层是整屏高度、时间线占剩下的空间，所以这里给产物一个自己的上限：展开一个
        // 几万行的 diff 不该把时间线压成一条缝。
        <div className="mt-1.5 max-h-[min(24rem,45vh)] space-y-2 overflow-y-auto rounded-md border bg-muted/20 p-2">
          {changes.length > 0 ? (
            <FileChangeList changes={changes} stats={statEntries} />
          ) : (
            <p className="px-1 text-caption text-muted-foreground">
              {t(($) => $.artifacts.empty_run)}
            </p>
          )}

          {diffFiles.map((file, index) => (
            <FileDiffBlock key={`${file.path}:${index}`} file={file} />
          ))}

          {truncated && (
            <p className="px-1 text-micro text-warning">
              {t(($) => $.artifacts.truncated_notice)}
            </p>
          )}
        </div>
      )}
    </div>
  );
}

// ─── 文件变更列表 ──────────────────────────────────────────────────────────

const CHANGE_TONE: Record<FileChange["kind"], string> = {
  add: "text-success",
  delete: "text-destructive",
  modify: "text-muted-foreground",
  rename: "text-info",
};

function FileChangeList({
  changes,
  stats,
}: {
  changes: readonly FileChange[];
  stats: readonly DiffStatEntry[];
}) {
  const { t } = useT("ruel");
  // --stat 是唯一能给到「每个文件各改了多少行」的来源；porcelain 只有状态没有行数。
  const byPath = useMemo(() => {
    const map = new Map<string, DiffStatEntry>();
    for (const entry of stats) map.set(entry.path, entry);
    return map;
  }, [stats]);

  return (
    <ul className="space-y-0.5">
      {changes.map((change) => (
        <li key={`${change.kind}:${change.path}`} className="flex items-center gap-2 px-1 text-caption">
          <span className={cn("shrink-0 text-micro uppercase", CHANGE_TONE[change.kind])}>
            {change.kind === "add"
              ? t(($) => $.artifacts.kind_add)
              : change.kind === "delete"
                ? t(($) => $.artifacts.kind_delete)
                : change.kind === "rename"
                  ? t(($) => $.artifacts.kind_rename)
                  : t(($) => $.artifacts.kind_modify)}
          </span>
          {change.fromPath ? (
            <span className="min-w-0 truncate font-mono text-micro">
              {t(($) => $.artifacts.renamed_to, { from: change.fromPath, to: change.path })}
            </span>
          ) : (
            <span className="min-w-0 truncate font-mono text-micro" title={change.path}>
              {change.path}
            </span>
          )}
          <FileStatBadge entry={byPath.get(change.path)} />
        </li>
      ))}
    </ul>
  );
}

function FileStatBadge({ entry }: { entry?: DiffStatEntry }) {
  const { t } = useT("ruel");
  if (!entry) return null;
  return (
    <span className="ml-auto shrink-0 font-mono text-micro tabular-nums text-faint-foreground">
      {entry.split
        ? t(($) => $.artifacts.lines, { added: entry.added, removed: entry.removed })
        : t(($) => $.artifacts.lines_only, { count: entry.changed })}
    </span>
  );
}

// ─── 单个文件的 diff ───────────────────────────────────────────────────────

function FileDiffBlock({ file }: { file: DiffFile }) {
  const { t } = useT("ruel");
  return (
    <div className="overflow-hidden rounded-md border bg-background">
      <div className="flex items-center gap-2 border-b px-2 py-1 font-mono text-micro">
        {file.isNew && (
          <span className="shrink-0 uppercase text-success">
            {t(($) => $.artifacts.kind_add)}
          </span>
        )}
        {file.isDeleted && (
          <span className="shrink-0 uppercase text-destructive">
            {t(($) => $.artifacts.kind_delete)}
          </span>
        )}
        <span className="min-w-0 truncate text-muted-foreground" title={file.path}>
          {file.path}
        </span>
      </div>
      {file.hunks.map((hunk, index) => (
        // 解析结果是一次性的、不再变化，所以下标可以当 key。
        <div key={`${hunk.header}:${index}`}>
          <div className="px-2 pt-1 font-mono text-micro text-faint-foreground">
            {hunk.header}
          </div>
          <DiffDetailSurface lines={hunk.lines} path={file.path} />
        </div>
      ))}
    </div>
  );
}
