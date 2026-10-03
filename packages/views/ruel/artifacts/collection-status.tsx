"use client";

// Ruel 新增：「这一轮为什么没有产物」这句话。
//
// 三种「产物为空」的情况在数据库里长得一模一样，但性质完全不同：没碰仓库和碰了没改动
// 是正常的，采集失败是缺陷。不做区分的话，缺陷就被伪装成「这轮没干活」——验收的人看
// 到「变更：无」只会以为 agent 摸鱼，不会想到是采集坏了。
//
// 单独成一个文件，是因为这句话有三个展示位（Run 弹层、Issue 侧栏的每一轮），写三遍
// 就会有三份各不相同的措辞。

import type { RuelCollectionStatus } from "@multica/core/ruel/artifacts";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";

export interface EmptyReasonProps {
  /** 采集结论。缺失表示连结论都没有——daemon 没上报成功，或者这轮早于采集功能。 */
  status?: RuelCollectionStatus | undefined;
  className?: string;
}

/**
 * 没有产物时的那句解释。
 *
 * 只有 collect_failed 用警示色：它是三种情况里唯一的缺陷，值得在视觉上跳出来说一句。
 * 另外两种是正常的，用普通弱化色就好——把正常的空也标红，警示色就没有意义了。
 */
export function EmptyReason({ status, className }: EmptyReasonProps) {
  const { t } = useT("ruel");

  // 默认弱化色由这里给，调用方只传布局类。反过来（调用方传颜色）会让警示色被覆盖——
  // cn 里同族类名后者胜出，一个 text-muted-foreground 就能把缺陷的警示色抹掉。
  if (status === "no_repo") {
    return (
      <span className={cn("text-muted-foreground", className)}>
        {t(($) => $.artifacts.reason_no_repo)}
      </span>
    );
  }
  if (status === "no_change") {
    return (
      <span className={cn("text-muted-foreground", className)}>
        {t(($) => $.artifacts.reason_no_change)}
      </span>
    );
  }
  if (status === "collect_failed") {
    return (
      <span className={cn("text-warning", className)}>
        {t(($) => $.artifacts.reason_collect_failed)}
      </span>
    );
  }
  // 没有结论可供解释时说保守的话：只陈述「没记录到变更」，不猜原因。
  return (
    <span className={cn("text-muted-foreground", className)}>
      {t(($) => $.artifacts.empty_run)}
    </span>
  );
}
