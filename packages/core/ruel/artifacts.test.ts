import { describe, expect, it } from "vitest";
import {
  collectionStatusOf,
  isCollectionStatus,
  splitRuelArtifacts,
  type RuelArtifact,
} from "./artifacts";

// 结论行与产物行在服务端是同一张表、同一个接口返回的，靠 kind 区分。分流错了，界面上
// 就会把「这轮为什么没有产物」当成一条产物去渲染——一个点开是空的变更区块。
function row(over: Partial<RuelArtifact> & { task_id: string; kind: string }): RuelArtifact {
  return {
    id: `${over.task_id}-${over.kind}`,
    uri: "",
    size: 0,
    checksum: "",
    content: "",
    created_at: "2026-10-03T00:00:00Z",
    ...over,
  };
}

describe("splitRuelArtifacts", () => {
  it("结论行不进产物列表，只进结论表", () => {
    const { items, statusByTask } = splitRuelArtifacts([
      row({ task_id: "t1", kind: "collection_status", content: "no_change" }),
      row({ task_id: "t1", kind: "diff", content: "+a" }),
    ]);
    expect(items).toHaveLength(1);
    expect(items[0]?.kind).toBe("diff");
    expect(statusByTask.get("t1")).toBe("no_change");
  });

  it("同一轮既有产物又有结论时两者都取得到", () => {
    const list = [
      row({ task_id: "t1", kind: "file_change", content: " M a.ts\n" }),
      row({ task_id: "t1", kind: "collection_status", content: "changed" }),
    ];
    const { items, statusByTask } = splitRuelArtifacts(list);
    expect(items).toHaveLength(1);
    expect(statusByTask.get("t1")).toBe("changed");
    expect(collectionStatusOf(list)).toBe("changed");
  });

  it("认不出的结论值当没有结论，而不是显示一个读不懂的词", () => {
    const list = [row({ task_id: "t1", kind: "collection_status", content: "no_changes" })];
    expect(splitRuelArtifacts(list).statusByTask.size).toBe(0);
    expect(collectionStatusOf(list)).toBeUndefined();
  });

  it("没有结论行时结论为 undefined——那是「压根没采集」，与「没改动」不同", () => {
    const list = [row({ task_id: "t1", kind: "diff", content: "+a" })];
    expect(collectionStatusOf(list)).toBeUndefined();
    expect(collectionStatusOf([])).toBeUndefined();
    expect(collectionStatusOf(undefined)).toBeUndefined();
  });

  it("按 task_id 分别记结论", () => {
    const { statusByTask } = splitRuelArtifacts([
      row({ task_id: "t1", kind: "collection_status", content: "no_repo" }),
      row({ task_id: "t2", kind: "collection_status", content: "collect_failed" }),
    ]);
    expect(statusByTask.get("t1")).toBe("no_repo");
    expect(statusByTask.get("t2")).toBe("collect_failed");
  });
});

describe("isCollectionStatus", () => {
  it("只认结论 kind", () => {
    expect(isCollectionStatus(row({ task_id: "t1", kind: "collection_status" }))).toBe(true);
    expect(isCollectionStatus(row({ task_id: "t1", kind: "diff" }))).toBe(false);
  });
});
