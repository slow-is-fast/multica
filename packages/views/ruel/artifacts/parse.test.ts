import { describe, expect, it } from "vitest";
import {
  countDiffLines,
  isTruncated,
  parseDiffStat,
  parseFileChanges,
  parseUnifiedDiff,
} from "./parse";

// 这些输入直接抄采集侧跑出来的样子：git status --porcelain、git diff --stat、
// git diff <base>。测试的价值在于把「解析对不对」和「采集对不对」分开——采集的
// 回归在 server/internal/daemon/ruel_artifacts_test.go。

const PORCELAIN = [
  "M  src/app.ts",
  "A  src/new-file.ts",
  "D  src/gone.ts",
  "R  src/old-name.ts -> src/new-name.ts",
  "?? src/untracked.ts",
  "3 files changed",
].join("\n");

describe("parseFileChanges", () => {
  it("把两位状态码翻译成四种变更类型", () => {
    const changes = parseFileChanges(PORCELAIN);
    expect(changes.map((c) => [c.kind, c.path])).toEqual([
      ["modify", "src/app.ts"],
      ["add", "src/new-file.ts"],
      ["delete", "src/gone.ts"],
      ["rename", "src/new-name.ts"],
      ["add", "src/untracked.ts"],
    ]);
  });

  it("重命名保留旧路径", () => {
    const rename = parseFileChanges("R  src/old-name.ts -> src/new-name.ts")[0];
    expect(rename?.fromPath).toBe("src/old-name.ts");
  });

  it("AM（加入后又改）算新增而不是修改", () => {
    // 判定顺序是重命名 → 删除 → 新增 → 修改。读者想知道的是「这个文件是本轮新建的」。
    expect(parseFileChanges("AM src/a.ts")[0]?.kind).toBe("add");
  });

  it("去掉 git 给带空格路径加的引号", () => {
    expect(parseFileChanges('A  "src/my file.ts"')[0]?.path).toBe("src/my file.ts");
  });

  it("空内容和汇总行都不产生条目", () => {
    expect(parseFileChanges("")).toEqual([]);
    expect(parseFileChanges("3 files changed, 2 insertions(+)")).toEqual([]);
  });
});

describe("parseDiffStat", () => {
  it("按文件拆出加减行数", () => {
    const entries = parseDiffStat(
      [" src/app.ts | 12 +++++---", " src/gone.ts | 4 ----", ""].join("\n"),
    );
    expect(entries).toEqual([
      { path: "src/app.ts", changed: 12, added: 5, removed: 3, split: true },
      { path: "src/gone.ts", changed: 4, added: 0, removed: 4, split: true },
    ]);
  });

  it("跳过末尾那行汇总", () => {
    expect(parseDiffStat("3 files changed, 21 insertions(+), 9 deletions(-)")).toEqual([]);
  });

  it("改动过大时 git 只给数字，此时不把加减当成 0", () => {
    // 这行是 git 在图形放不下时的真实输出：只有总数，没有 +/-。
    const [entry] = parseDiffStat(" src/huge.ts | 5000 ++++++++++++++++++++++...");
    // 这里仍有图形，验证正常路径；下面那行才是无图形的退化形态。
    expect(entry?.split).toBe(true);

    const [noGraph] = parseDiffStat(" src/huge.ts | 5000");
    expect(noGraph).toEqual({
      path: "src/huge.ts",
      changed: 5000,
      added: 0,
      removed: 0,
      split: false,
    });
  });

  it("重命名取箭头后面的新路径", () => {
    const [entry] = parseDiffStat(" src/{old => new}.ts | 2 +-");
    expect(entry?.path).toBe("src/{old => new}.ts".replace("{old => new}", "new"));
  });
});

const DIFF = [
  "diff --git a/src/app.ts b/src/app.ts",
  "--- a/src/app.ts",
  "+++ b/src/app.ts",
  "@@ -1,4 +1,5 @@",
  " import x",
  "-const a = 1",
  "+const a = 2",
  "+const b = 3",
  " export default a",
  "diff --git a/src/new.ts b/src/new.ts",
  "new file mode 100644",
  "--- /dev/null",
  "+++ b/src/new.ts",
  "@@ -0,0 +1 @@",
  "+hello",
].join("\n");

describe("parseUnifiedDiff", () => {
  it("按文件切开并认出路径", () => {
    const files = parseUnifiedDiff(DIFF);
    expect(files.map((f) => f.path)).toEqual(["src/app.ts", "src/new.ts"]);
  });

  it("认出新文件", () => {
    const files = parseUnifiedDiff(DIFF);
    expect(files[0]?.isNew).toBe(false);
    expect(files[1]?.isNew).toBe(true);
  });

  it("hunk 头不混进 diff 行里", () => {
    const file = parseUnifiedDiff(DIFF)[0];
    expect(file?.hunks).toHaveLength(1);
    expect(file?.hunks[0]?.header).toBe("@@ -1,4 +1,5 @@");
    expect(file?.hunks[0]?.lines.map((l) => `${l.kind}:${l.text}`)).toEqual([
      "context:import x",
      "remove:const a = 1",
      "add:const a = 2",
      "add:const b = 3",
      "context:export default a",
    ]);
  });

  it("\\ No newline 那一行被丢掉而不是当成代码", () => {
    const file = parseUnifiedDiff(
      ["diff --git a/a b/a", "--- a/a", "+++ b/a", "@@ -1 +1 @@", "-old", "\\ No newline at end of file", "+new"].join("\n"),
    )[0];
    expect(file?.hunks[0]?.lines.map((l) => l.kind)).toEqual(["remove", "add"]);
  });

  it("数加减行数", () => {
    const { added, removed } = countDiffLines(parseUnifiedDiff(DIFF));
    // 第一个文件 +2/-1，第二个文件 +1/-0。
    expect(added).toBe(3);
    expect(removed).toBe(1);
  });

  it("删除的文件靠 --- 那一侧拿到路径", () => {
    // 删除时 `+++` 是 /dev/null，只有 `---` 还带着路径。
    const files = parseUnifiedDiff(
      [
        "diff --git a/src/gone.ts b/src/gone.ts",
        "deleted file mode 100644",
        "--- a/src/gone.ts",
        "+++ /dev/null",
        "@@ -1 +0,0 @@",
        "-const a = 1",
      ].join("\n"),
    );
    expect(files[0]?.path).toBe("src/gone.ts");
    expect(files[0]?.isDeleted).toBe(true);
    expect(files[0]?.isNew).toBe(false);
  });

  it("两次改动之间过长的上下文折叠成 gap，两端各留几行", () => {
    const lines = ["@@ -1,40 +1,42 @@"];
    for (let i = 0; i < 12; i += 1) lines.push(` context-head-${i}`);
    lines.push("+added-1");
    for (let i = 0; i < 12; i += 1) lines.push(` context-middle-${i}`);
    lines.push("+added-2");
    for (let i = 0; i < 12; i += 1) lines.push(` context-tail-${i}`);
    const file = parseUnifiedDiff(
      ["diff --git a/big b/big", "--- a/big", "+++ b/big", ...lines].join("\n"),
    )[0];
    const kinds = file?.hunks[0]?.lines.map((l) => l.kind) ?? [];
    // 夹在两次改动中间的那段被压成一行 gap。
    expect(kinds).toContain("gap");
    // 首段和尾段不折叠——它们回答的是「这段改动落在文件的什么位置」。
    expect(kinds.at(-1)).toBe("context");
    expect(kinds.filter((k) => k === "add")).toHaveLength(2);
    // 折叠确实省下了行：原本 36 行上下文，现在少了一大截。
    expect(kinds.filter((k) => k === "context").length).toBeLessThan(36);
  });

  it("只有一处改动时不折叠", () => {
    // 首尾那两段上下文就是位置信息，压掉反而读不出改动落在哪里。
    const lines = ["@@ -1,20 +1,21 @@"];
    for (let i = 0; i < 12; i += 1) lines.push(` context-${i}`);
    lines.push("+added");
    for (let i = 0; i < 12; i += 1) lines.push(` context-tail-${i}`);
    const file = parseUnifiedDiff(
      ["diff --git a/big b/big", "--- a/big", "+++ b/big", ...lines].join("\n"),
    )[0];
    const kinds = file?.hunks[0]?.lines.map((l) => l.kind) ?? [];
    expect(kinds).not.toContain("gap");
    expect(kinds.filter((k) => k === "context")).toHaveLength(24);
  });

  it("没有 diff --git 头时整段当一个文件处理，而不是什么都不显示", () => {
    const files = parseUnifiedDiff("@@ -1 +1 @@\n-old\n+new");
    expect(files).toHaveLength(1);
    expect(countDiffLines(files)).toEqual({ added: 1, removed: 1 });
  });
});

describe("isTruncated", () => {
  it("认出采集侧的截断标记", () => {
    expect(isTruncated("+a\n\n[ruel] 内容超过 1048576 字节已截断")).toBe(true);
    expect(isTruncated("+a")).toBe(false);
  });
});
