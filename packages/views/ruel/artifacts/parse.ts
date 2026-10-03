// Ruel 新增：把 daemon 采集到的 git 输出解析成可渲染的结构。
//
// 采集侧（server/internal/daemon/ruel_artifacts.go）跑的是三条命令：
//   git diff <base>          → kind = diff         统一 diff 全文
//   git diff <base> --stat   → kind = diff_stat    每个文件改了多少行
//   git status --porcelain   → kind = file_change  改了哪些文件
//
// 三条命令的输出格式是稳定的，但「稳定」不等于「不需要容错」：diff 可能被截断
// （采集侧有 1 MiB 上限），路径可能被引号包裹（core.quotePath），超大的文件 git
// 只给数字不给图形。这里把每种退化都显式表达出来，而不是让它悄悄渲染成错的数。

/** 与上游 TraceDiffLine 形状一致，好让产物直接复用上游的 diff 渲染面。 */
export interface DiffLine {
  kind: "add" | "remove" | "context" | "gap";
  text: string;
  /** `gap` 代表省略了多少行上下文；其他类型没有这个字段。 */
  hidden?: number;
}

export type FileChangeKind = "add" | "delete" | "modify" | "rename";

export interface FileChange {
  kind: FileChangeKind;
  path: string;
  /** 重命名时的旧路径，其余类型没有。 */
  fromPath?: string;
}

export interface DiffStatEntry {
  path: string;
  /** 这个文件改了多少行（git 给的总数）。 */
  changed: number;
  added: number;
  removed: number;
  /**
   * false 表示 git 没有输出 +/- 图形（改动太大时它只给总数），此时 added/removed
   * 拆不出来，界面要显示总数而不是一个假的 0。
   */
  split: boolean;
}

export interface DiffHunk {
  /** `@@ -1,4 +1,6 @@` 这一行原文。 */
  header: string;
  lines: DiffLine[];
}

export interface DiffFile {
  path: string;
  /** true 表示这是个新文件（`--- /dev/null`）。 */
  isNew: boolean;
  /** true 表示这是个删除（`+++ /dev/null`）。 */
  isDeleted: boolean;
  hunks: DiffHunk[];
}

/** 采集侧截断标记。出现在内容里就说明这只是完整 diff 的前 1 MiB。 */
const TRUNCATION_MARKER = "[ruel] 内容超过";

export function isTruncated(content: string): boolean {
  return content.includes(TRUNCATION_MARKER);
}

// ─── git status --porcelain ────────────────────────────────────────────────

/**
 * 解析 `git status --porcelain`。
 *
 * 每行的前两位是索引区和工作区各自的状态，第三位是空格，剩下的是路径；重命名写作
 * `R  old -> new`。
 *
 * 判定顺序是重命名 → 删除 → 新增 → 修改。这样 `AM`（加入后又改）算新增而不是修改：
 * 「这个文件是本轮新建的」比「这个文件被改过」更贴近读者想知道的事。
 */
export function parseFileChanges(content: string): FileChange[] {
  const out: FileChange[] = [];
  for (const rawLine of content.split("\n")) {
    const line = rawLine.replace(/\r$/, "");
    if (line.length < 4 || line[2] !== " ") continue;
    const indexStatus = line[0];
    const worktreeStatus = line[1];
    const rest = line.slice(3);
    if (rest === "") continue;

    const code = `${indexStatus}${worktreeStatus}`;
    let kind: FileChangeKind;
    if (code.includes("R")) kind = "rename";
    else if (code.includes("D")) kind = "delete";
    else if (code.includes("A") || code.includes("?") || code.includes("C")) kind = "add";
    else kind = "modify";

    if (kind === "rename") {
      // `old -> new`，路径里也可能有 " -> "，所以按最后一次出现切分。
      const at = rest.lastIndexOf(" -> ");
      if (at > 0) {
        out.push({
          kind,
          path: unquotePath(rest.slice(at + 4)),
          fromPath: unquotePath(rest.slice(0, at)),
        });
        continue;
      }
    }
    out.push({ kind, path: unquotePath(rest) });
  }
  return out;
}

/**
 * 去掉 git 给路径加的引号。
 *
 * core.quotePath 默认开启，路径里有空格或非 ASCII 时 git 会把它写成 C 风格的字符串。
 * 先按 JSON 试一次（能正确还原 \n、\" 这类转义）；失败就只去掉外层引号。八进制转义
 * （中文路径常被写成 \344\275\240）这里不还原——路径仍然可读，只是不完美，而为了
 * 一个显示用的路径去做完整的字节解码不值得。
 */
function unquotePath(value: string): string {
  if (value.length < 2 || !value.startsWith('"') || !value.endsWith('"')) return value;
  try {
    const parsed: unknown = JSON.parse(value);
    return typeof parsed === "string" ? parsed : value;
  } catch {
    return value.slice(1, -1);
  }
}

// ─── git diff --stat ───────────────────────────────────────────────────────

/**
 * 解析 `git diff --stat`。
 *
 * 形如：
 *   src/app.ts | 12 +++++---
 *   3 files changed, 21 insertions(+), 9 deletions(-)
 *
 * 最后那行汇总没有 `|`，直接跳过。
 */
export function parseDiffStat(content: string): DiffStatEntry[] {
  const out: DiffStatEntry[] = [];
  for (const rawLine of content.split("\n")) {
    const line = rawLine.replace(/\r$/, "");
    const bar = line.indexOf("|");
    if (bar < 0) continue;

    const rawPath = line.slice(0, bar).trim();
    if (rawPath === "") continue;
    const path = statPath(rawPath);

    const tail = line.slice(bar + 1).trim();
    const match = /^(\d+)\s*(.*)$/.exec(tail);
    const changed = match?.[1] ? Number(match[1]) : 0;
    const graph = match?.[2] ?? "";
    const added = countChar(graph, "+");
    const removed = countChar(graph, "-");
    out.push({
      path,
      changed,
      added,
      removed,
      // 改动太多时 git 省略图形，只剩数字；这时不能把 added/removed 当成 0。
      split: added + removed > 0,
    });
  }
  return out;
}

/**
 * 把 --stat 里的重命名路径还原成新路径。
 *
 * git 有两种写法，取决于改动是否小到能放进一行：
 *   src/{old => new}.ts   —— 花括号只包住变化的那一段，前后缀共用
 *   src/old.ts => src/new.ts —— 整条路径都写出来
 * 取新路径是因为读者关心的是「现在叫这个名字」，旧名字在 porcelain 的变更列表里
 * 另有体现。
 */
function statPath(rawPath: string): string {
  const braced = /^(.*)\{([^}]*?)\s*=>\s*([^}]*?)\}(.*)$/.exec(rawPath);
  if (braced) return `${braced[1] ?? ""}${braced[3] ?? ""}${braced[4] ?? ""}`;
  const arrow = rawPath.lastIndexOf(" => ");
  return arrow > 0 ? rawPath.slice(arrow + 4) : rawPath;
}

function countChar(value: string, char: string): number {
  let n = 0;
  for (let i = 0; i < value.length; i += 1) if (value[i] === char) n += 1;
  return n;
}

// ─── git diff ──────────────────────────────────────────────────────────────

/** 一个 hunk 里保留的上下文行数；超出的折叠成 gap。 */
const CONTEXT_KEEP = 3;
/** 连续上下文超过多少行才值得折叠。太小的话折叠本身就比省下的行还多。 */
const CONTEXT_COLLAPSE_THRESHOLD = 8;

/**
 * 把统一 diff 全文切成「按文件 + 按 hunk」的结构。
 *
 * 按文件切是因为一次 Run 常常动好几个文件，糊成一坨就分不清哪段改动属于哪个文件；
 * 按 hunk 切是因为 `@@` 行本身不是代码，混进 diff 行里会在左边多出一个空格，读起来
 * 像缩进了一级。
 */
export function parseUnifiedDiff(content: string): DiffFile[] {
  const files: DiffFile[] = [];
  const chunks = content.split(/^diff --git /m).slice(1);
  if (chunks.length === 0 && content.trim() !== "") {
    // 没有 `diff --git` 头（比如只有一段裸 diff），整段当成一个无名文件处理，
    // 免得明明有内容却什么都不显示。
    const lines = parseHunks(content);
    if (lines.length > 0) files.push({ path: "", isNew: false, isDeleted: false, hunks: lines });
    return files;
  }

  for (const chunk of chunks) {
    const body = `diff --git ${chunk}`;
    const lines = body.split("\n");
    let oldPath = "";
    let newPath = "";
    let renamedTo = "";
    for (const line of lines) {
      if (line.startsWith("--- ") && oldPath === "") {
        const value = line.slice(4).trim();
        oldPath = value === "/dev/null" ? "" : stripPrefix(value);
      } else if (line.startsWith("+++ ") && newPath === "") {
        const value = line.slice(4).trim();
        newPath = value === "/dev/null" ? "" : stripPrefix(value);
      } else if (line.startsWith("rename to ")) {
        renamedTo = line.slice("rename to ".length).trim();
      }
    }
    // 删除的文件 `+++` 是 /dev/null，新增的文件 `---` 是 /dev/null——只能靠另一边
    // 拿到路径。两个都是空说明这个 chunk 没有可展示的文件头，跳过。
    const path = renamedTo || newPath || oldPath;
    if (path === "") continue;
    const isNew = oldPath === "" && newPath !== "";
    const isDeleted = newPath === "" && oldPath !== "";
    const hunks = parseHunks(body);
    if (hunks.length === 0) continue;
    files.push({ path, isNew, isDeleted, hunks });
  }
  return files;
}

/** `a/src/app.ts` / `b/src/app.ts` → `src/app.ts`。 */
function stripPrefix(value: string): string {
  if (value === "/dev/null") return "";
  return value.replace(/^[ab]\//, "");
}

function parseHunks(body: string): DiffHunk[] {
  const hunks: DiffHunk[] = [];
  let current: DiffHunk | null = null;
  let raw: DiffLine[] = [];

  const flush = () => {
    if (current) hunks.push({ header: current.header, lines: collapseContext(raw) });
    raw = [];
  };

  for (const rawLine of body.split("\n")) {
    const line = rawLine.replace(/\r$/, "");
    if (line.startsWith("@@")) {
      flush();
      current = { header: line, lines: [] };
      continue;
    }
    if (!current) continue;
    if (line.startsWith("+")) raw.push({ kind: "add", text: line.slice(1) });
    else if (line.startsWith("-")) raw.push({ kind: "remove", text: line.slice(1) });
    else if (line.startsWith(" ")) raw.push({ kind: "context", text: line.slice(1) });
    else if (line.startsWith("\\")) {
      // "\ No newline at end of file" —— 不是代码行，跳过。
      continue;
    } else {
      // 走到这里说明 hunk 结束了（遇到了下一个文件的头或 diff 结尾）。
      flush();
      current = null;
    }
  }
  flush();
  return hunks;
}

/**
 * 把一段 hunk 中间过长的上下文折叠成一行 gap。
 *
 * 一个几百行的文件里改两行，不折叠的话读者要在无关代码里找那两行；全折叠又看不出
 * 改动落在文件的什么位置。所以两端各留几行，中间压成一行「⋯ 省略 N 行」。
 */
function collapseContext(lines: DiffLine[]): DiffLine[] {
  const contextRuns: { start: number; end: number }[] = [];
  let runStart = -1;
  for (let i = 0; i < lines.length; i += 1) {
    if (lines[i]?.kind === "context") {
      if (runStart < 0) runStart = i;
    } else if (runStart >= 0) {
      contextRuns.push({ start: runStart, end: i });
      runStart = -1;
    }
  }
  if (runStart >= 0) contextRuns.push({ start: runStart, end: lines.length });

  // 首尾的上下文是「这段改动在文件的哪个位置」，只有中间的段落才值得折叠。
  const collapsible = contextRuns.slice(1, -1).filter((r) => r.end - r.start > CONTEXT_COLLAPSE_THRESHOLD);
  if (collapsible.length === 0) return lines;

  const out: DiffLine[] = [];
  let cursor = 0;
  for (const run of collapsible) {
    out.push(...lines.slice(cursor, run.start + CONTEXT_KEEP));
    const hidden = run.end - run.start - CONTEXT_KEEP * 2;
    out.push({ kind: "gap", text: "", hidden });
    cursor = run.end - CONTEXT_KEEP;
  }
  out.push(...lines.slice(cursor));
  return out;
}

/** 数一次 Run 的 diff 里加减了多少行，用于折叠状态下的摘要。 */
export function countDiffLines(files: readonly DiffFile[]): { added: number; removed: number } {
  let added = 0;
  let removed = 0;
  for (const file of files) {
    for (const hunk of file.hunks) {
      for (const line of hunk.lines) {
        if (line.kind === "add") added += 1;
        else if (line.kind === "remove") removed += 1;
      }
    }
  }
  return { added, removed };
}
