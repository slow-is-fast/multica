/**
 * Ruel 新增：把仓库根目录的 LICENSE / NOTICE 原文通过 HTTP 送出去。
 *
 * 为什么不是复制一份到 `public/`：Apache 2.0 §4(a) 要求随附的是「原文」。复制品一旦
 * 与上游文件各自漂移（上游改了 NOTICE、我们没同步），送出去的就不是原文了——那比不放
 * 链接更糟，因为它看起来是合规的。所以这里直接读根目录那一份，单一事实来源。
 *
 * 为什么用白名单而不是把参数拼进路径：这是一个被外部输入驱动的文件读取。白名单把可读
 * 范围钉死在两个固定文件名上，`..`、绝对路径、符号链接逃逸一概进不来。
 */

import { readFile } from "node:fs/promises";
import { existsSync } from "node:fs";
import path from "node:path";

// 与 favicon 那条路由同一个理由：这个响应不该进预渲染缓存。见
// apps/web/app/favicon.ico/route.ts 的说明。
export const dynamic = "force-dynamic";

const ALLOWED: Record<string, string> = {
  LICENSE: "LICENSE",
  NOTICE: "NOTICE",
};

/**
 * 从当前工作目录往上找仓库根。
 *
 * `next dev` / `next start` 的 cwd 是 apps/web，而打包部署时可能就是仓库根；写死任何
 * 一层都会在某一种运行方式下失效。判据是「同时存在 LICENSE 和 NOTICE 的那一层」——
 * 这个组合在子目录里不会出现，向上最多找 6 层，找不到就明确拒绝而不是猜。
 */
function findRepoRoot(from: string): string | null {
  let dir = path.resolve(from);
  for (let i = 0; i < 6; i += 1) {
    if (
      existsSync(path.join(dir, "LICENSE")) &&
      existsSync(path.join(dir, "NOTICE"))
    ) {
      return dir;
    }
    const parent = path.dirname(dir);
    if (parent === dir) break;
    dir = parent;
  }
  return null;
}

export async function GET(
  _request: Request,
  { params }: { params: Promise<{ name: string }> },
) {
  const { name } = await params;
  const file = ALLOWED[name];
  if (!file) {
    return new Response("Not found", { status: 404 });
  }

  const root = findRepoRoot(process.cwd());
  if (!root) {
    return new Response("LICENSE / NOTICE not found at the repository root", {
      status: 404,
    });
  }

  const target = path.join(root, file);
  const body = await readFile(target, "utf8");
  return new Response(body, {
    headers: {
      "Content-Type": "text/plain; charset=utf-8",
      "Cache-Control": "public, max-age=3600",
    },
  });
}
