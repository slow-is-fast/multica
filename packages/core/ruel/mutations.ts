// Ruel 新增：项目知识审批的 mutation（#48）。
//
// 与 queries.ts 同一层的邻居，形状照 issues/mutations.ts 的写法：一个 hook 一个
// mutation，失败不在这里吞——调用方要按错误码给中文提示，而错误码只有它拿得到。
//
// 两个 hook 都收 workspace id：缓存 key 里有它，作废时必须用同一个，否则点完批准
// 队列不会刷新（作废的是另一个 key），而界面看起来只是「点了没反应」。

import { useQueryClient, useMutation } from "@tanstack/react-query";
import { api } from "../api";
import type { RuelKnowledgeEntry } from "./knowledge";
import { ruelKnowledgeKeys } from "./queries";

/**
 * 批准一条待审候选。
 *
 * 批准不带理由：批准本身已经把候选推进了库里，界面再要一个输入框只是多一步。
 * 服务端也允许 body 里没有 note，所以这里整块 body 都不发。
 */
export function useApproveRuelKnowledge(wsId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ id }: { id: string }) => api.approveRuelKnowledge(id),
    ...knowledgeReviewed(client, wsId),
  });
}

/** 拒绝一条待审候选。理由由服务端强制（空理由会被 400 挡回来），界面也先挡一道。 */
export function useRejectRuelKnowledge(wsId: string) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ id, note }: { id: string; note: string }) =>
      api.rejectRuelKnowledge(id, note),
    ...knowledgeReviewed(client, wsId),
  });
}

/**
 * 审批成功后的缓存处理，批准与拒绝共用。
 *
 * 两件事，顺序有意：
 *
 * 1. **先把这条从队列里去掉**。队列回答的是「还有什么等着人看」，本条已经有结论了。
 *    服务端特意回整条记录就是为了这一步——不用等一次网络往返才让界面跟上。
 * 2. **再作废队列**，让服务端重新说一遍话。作废不能省：并发时另一个审批人可能刚
 *    处理了别的条目，本地这份已经旧了。
 *
 * onSettled 而不是 onSuccess：失败的那次也要作废。409（这条已经被别人处理过了）
 * 是最典型的一种——本地队列正是过期的，不刷新就会一直显示一条批不动的候选。
 */
function knowledgeReviewed(
  client: ReturnType<typeof useQueryClient>,
  wsId: string,
) {
  return {
    onSuccess: (entry: RuelKnowledgeEntry | undefined) => {
      if (!entry) return;
      client.setQueryData<RuelKnowledgeEntry[]>(
        ruelKnowledgeKeys.pending(wsId),
        (rows) => rows?.filter((row) => row.id !== entry.id),
      );
    },
    onSettled: () => {
      void client.invalidateQueries({ queryKey: ruelKnowledgeKeys.pending(wsId) });
    },
  };
}
