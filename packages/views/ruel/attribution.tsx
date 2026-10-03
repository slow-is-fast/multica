"use client";

// Ruel 新增：复用上游前端的三项署名义务在界面上的落脚点。
//
// LICENSE Part I 有三条与我们相关的要求：
//   1(b) 不得删除 Multica 界面上显示的版权与署名信息；
//   1(c) 不以界面形式使用时，要在用户可见的文档里说明「built on Multica」并给出
//        上游仓库链接；
//   Part II（Apache 2.0 §4a）要求随附 LICENSE 与 NOTICE 原文。
//
// 前两条文件我们一直留在仓库根目录，但「留着」不等于「看得到」——验收的人不会去翻
// 仓库。所以这里给它们各一个入口：署名文字本身链到上游仓库，LICENSE 与 NOTICE 链到
// 由 apps/web 直接从仓库根目录读出来的原文（见 apps/web/app/licenses/[name]/route.ts），
// 而不是复制一份到 public/——复制品会和上游文件各自漂移，那才是真正的合规风险。

import { useT } from "../i18n";

const UPSTREAM_REPO = "https://github.com/multica-ai/multica";
const LICENSE_HREF = "/licenses/LICENSE";
const NOTICE_HREF = "/licenses/NOTICE";

export function RuelAttribution() {
  const { t } = useT("ruel");
  return (
    <p className="px-1 pt-1 text-micro text-faint-foreground">
      <a
        href={UPSTREAM_REPO}
        target="_blank"
        rel="noreferrer"
        className="underline decoration-dotted underline-offset-2 transition-colors hover:text-muted-foreground"
      >
        {t(($) => $.attribution.built_on)}
      </a>
      <span aria-hidden> · </span>
      <a
        href={LICENSE_HREF}
        target="_blank"
        rel="noreferrer"
        className="transition-colors hover:text-muted-foreground"
      >
        {t(($) => $.attribution.license)}
      </a>
      <span aria-hidden> · </span>
      <a
        href={NOTICE_HREF}
        target="_blank"
        rel="noreferrer"
        className="transition-colors hover:text-muted-foreground"
      >
        {t(($) => $.attribution.notice)}
      </a>
    </p>
  );
}
