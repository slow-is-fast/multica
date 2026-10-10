"use client";

// Ruel 新增：项目知识候选的待审队列（#48）。
//
// PRD 第 5 章写的是「初版避免自动全局记忆」——知识必须经人批准才进库。界面这一侧
// 因此只有两件事：把候选摆出来让人判断，把人的判断送回去。
//
// 三处刻意的取舍，都不是样式问题：
//
// 1. **队列为零也要显示「待审 0 条」。** 静默最常见的形态是「没有问题的时候什么都
//    不显示」（#40 那条纪律）：那时人分不清「都审完了」和「这个功能不见了/坏了」。
//    但「还不知道」不能说成 0——加载中与请求失败都不显示数字，报 0 就是在说
//    「都审完了」，而这个功能的全部意义就是让人相信那件事没发生。
//
// 2. **拒绝必须写理由。** 留痕 = 记人 + 记时间 + 记为什么；少了为什么，下一个人
//    （或下一个提案的 agent）只看到「被拒了」，还得从头判断一遍。所以拒绝是就地
//    展开一个理由框，理由为空时确认按钮不可点——不靠后端绕一圈再拿回一个 400。
//
// 3. **失败提示按机器可读码翻成中文。** 服务端用 writeErrorCode 给每个可预期的
//    失败挂了码，正是为了这一刻；认不出的码退回一句中文泛化提示，而不是把服务端
//    那句英文抛给中文用户。

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { AlertCircle, Check, Loader2 } from "lucide-react";
import { toast } from "sonner";
import { errorCode } from "@multica/core/api";
import { useWorkspaceId } from "@multica/core/hooks";
import { useApproveRuelKnowledge, useRejectRuelKnowledge } from "@multica/core/ruel/mutations";
import { knowledgeSourceOf, type RuelKnowledgeEntry } from "@multica/core/ruel/knowledge";
import { pendingKnowledgeOptions } from "@multica/core/ruel/queries";
import { useWorkspacePaths } from "@multica/core/paths";
import { Badge } from "@multica/ui/components/ui/badge";
import { Button } from "@multica/ui/components/ui/button";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@multica/ui/components/ui/empty";
import { Textarea } from "@multica/ui/components/ui/textarea";
import { AppLink } from "../navigation";
import { SettingsTab } from "../settings/components/settings-layout";
import { useT } from "../i18n";

export function KnowledgeApprovalTab() {
  const { t } = useT("ruel");
  const wsId = useWorkspaceId();
  const { data, isPending, isError, refetch } = useQuery(pendingKnowledgeOptions(wsId));
  const entries = data ?? [];
  // 「还不知道」与「一条都没有」必须分开：只有后者能报 0。
  const count = isPending || isError ? undefined : entries.length;

  return (
    <SettingsTab
      title={t(($) => $.knowledge.title)}
      description={t(($) => $.knowledge.description)}
      scope="workspace"
      actions={
        count === undefined ? null : (
          <Badge variant="outline">{t(($) => $.knowledge.queue_count, { count })}</Badge>
        )
      }
    >
      {isError ? (
        <Empty>
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <AlertCircle aria-hidden="true" />
            </EmptyMedia>
            <EmptyTitle>{t(($) => $.knowledge.load_failed_title)}</EmptyTitle>
            <EmptyDescription>{t(($) => $.knowledge.load_failed_description)}</EmptyDescription>
          </EmptyHeader>
          <EmptyContent>
            <Button type="button" size="sm" variant="outline" onClick={() => void refetch()}>
              {t(($) => $.knowledge.retry)}
            </Button>
          </EmptyContent>
        </Empty>
      ) : isPending ? (
        <p role="status" className="flex items-center gap-2 text-body text-muted-foreground">
          <Loader2 aria-hidden="true" className="size-4 motion-safe:animate-spin" />
          {t(($) => $.knowledge.loading)}
        </p>
      ) : entries.length === 0 ? (
        <Empty>
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <Check aria-hidden="true" />
            </EmptyMedia>
            <EmptyTitle>{t(($) => $.knowledge.empty_title)}</EmptyTitle>
            <EmptyDescription>{t(($) => $.knowledge.empty_description)}</EmptyDescription>
          </EmptyHeader>
        </Empty>
      ) : (
        <ul className="flex flex-col gap-3">
          {entries.map((entry) => (
            <KnowledgeRow key={entry.id} entry={entry} />
          ))}
        </ul>
      )}
    </SettingsTab>
  );
}

function KnowledgeRow({ entry }: { entry: RuelKnowledgeEntry }) {
  const { t } = useT("ruel");
  const wsId = useWorkspaceId();
  const paths = useWorkspacePaths();
  const approve = useApproveRuelKnowledge(wsId);
  const reject = useRejectRuelKnowledge(wsId);
  const failureMessage = useReviewFailureMessage();

  const [rejecting, setRejecting] = useState(false);
  const [note, setNote] = useState("");
  const busy = approve.isPending || reject.isPending;
  // 理由去首尾空白后再判空：一串空格不是理由，服务端也会把它当空的 400 挡回来。
  // 在按钮上就挡住，人不用先失败一次才知道规则。
  const canReject = note.trim().length > 0;

  const source = knowledgeSourceOf(entry);

  return (
    <li className="rounded-lg border border-border bg-card p-3.5">
      <div className="flex items-start justify-between gap-3">
        <p className="min-w-0 flex-1 text-body font-medium text-foreground">{entry.statement}</p>
        <div className="flex shrink-0 items-center gap-1.5">
          <Button
            type="button"
            size="xs"
            variant="outline"
            disabled={busy}
            onClick={() =>
              approve.mutate(
                { id: entry.id },
                {
                  onSuccess: () => toast.success(t(($) => $.knowledge.approved_toast)),
                  onError: (err) => toast.error(failureMessage(err)),
                },
              )
            }
          >
            {approve.isPending ? (
              <Loader2 aria-hidden="true" className="motion-safe:animate-spin" />
            ) : null}
            {t(($) => $.knowledge.approve)}
          </Button>
          <Button
            type="button"
            size="xs"
            variant="outline"
            disabled={busy || rejecting}
            onClick={() => setRejecting(true)}
          >
            {t(($) => $.knowledge.reject)}
          </Button>
        </div>
      </div>

      {entry.rationale ? (
        <p className="mt-1.5 text-body/relaxed text-muted-foreground">{entry.rationale}</p>
      ) : null}

      <p className="mt-2 text-caption text-muted-foreground">
        {source?.kind === "issue" ? (
          <AppLink
            href={paths.issueDetail(source.id)}
            className="text-primary underline-offset-4 hover:underline"
          >
            {t(($) => $.knowledge.source_issue)}
          </AppLink>
        ) : source?.kind === "comment" ? (
          t(($) => $.knowledge.source_comment)
        ) : source?.kind === "task" ? (
          t(($) => $.knowledge.source_task)
        ) : (
          // 三个来源都为空：溯源的记录本身消失了。知识刻意不为它设外键，所以这是
          // 一种正常状态，明说「来源不详」而不是留一个空白——空白看起来像界面坏了。
          t(($) => $.knowledge.source_unknown)
        )}
      </p>

      {rejecting ? (
        <div className="mt-2.5 space-y-2 border-t border-border pt-2.5">
          <label
            className="block text-caption font-medium text-foreground"
            htmlFor={`reject-note-${entry.id}`}
          >
            {t(($) => $.knowledge.reject_reason_label)}
          </label>
          <Textarea
            id={`reject-note-${entry.id}`}
            rows={2}
            value={note}
            disabled={reject.isPending}
            placeholder={t(($) => $.knowledge.reject_reason_placeholder)}
            onChange={(event) => setNote(event.target.value)}
          />
          <div className="flex items-center gap-1.5">
            <Button
              type="button"
              size="xs"
              variant="destructive"
              disabled={!canReject || busy}
              onClick={() =>
                reject.mutate(
                  { id: entry.id, note: note.trim() },
                  {
                    onSuccess: () => toast.success(t(($) => $.knowledge.rejected_toast)),
                    onError: (err) => toast.error(failureMessage(err)),
                  },
                )
              }
            >
              {reject.isPending ? (
                <Loader2 aria-hidden="true" className="motion-safe:animate-spin" />
              ) : null}
              {t(($) => $.knowledge.reject_confirm)}
            </Button>
            <Button
              type="button"
              size="xs"
              variant="ghost"
              disabled={busy}
              onClick={() => {
                setRejecting(false);
                setNote("");
              }}
            >
              {t(($) => $.knowledge.reject_cancel)}
            </Button>
          </div>
        </div>
      ) : null}
    </li>
  );
}

/**
 * 把服务端给的失败码翻成界面上的话。
 *
 * 这是 writeErrorCode 存在的理由：服务端那句英文（"a rejection must carry a reason"）
 * 给到中文用户就是一句噪音，而它同时挂了机器可读的码，界面据此说自己的语言。
 *
 * 认不出的码（老服务端、或者将来新增的码）退回一句中文泛化提示，而不是 err.message
 * ——服务端的 message 是给日志用的，直接显示等于把内部措辞泄到界面上。
 */
function useReviewFailureMessage() {
  const { t } = useT("ruel");
  return (err: unknown): string => {
    switch (errorCode(err)) {
      case "knowledge_already_reviewed":
        return t(($) => $.knowledge.failed_already_reviewed);
      case "knowledge_not_found":
        return t(($) => $.knowledge.failed_not_found);
      case "knowledge_reject_note_required":
        return t(($) => $.knowledge.failed_note_required);
      default:
        return t(($) => $.knowledge.failed_generic);
    }
  };
}
