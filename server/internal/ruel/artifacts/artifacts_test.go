package artifacts

import (
	"context"
	"testing"
)

// 结论值要收紧：它是界面上那句「这轮为什么没有产物」的索引，放进来一个没见过的词，
// 前端只能显示空白，缺陷就又被藏回去了。所以未知结论必须在写库之前被挡掉。
func TestValidStatus(t *testing.T) {
	for _, status := range []string{
		StatusChanged, StatusNoRepo, StatusNoChange, StatusCollectFailed,
	} {
		if !ValidStatus(status) {
			t.Errorf("已知结论 %q 应当通过校验", status)
		}
	}

	for _, status := range []string{"", " ", "no_changes", "CHANGED", "unknown"} {
		if ValidStatus(status) {
			t.Errorf("未知结论 %q 不该通过校验", status)
		}
	}
}

// db 传 nil 是刻意的：非法结论必须在碰到数据库之前就被拒绝，否则一条拼错的状态写进
// 表里，界面上就是一句读不懂的空话，而那条 Run 的真实情况永远没人知道。
func TestUpsertStatusRejectsUnknown(t *testing.T) {
	store := NewStore(nil)
	err := store.UpsertStatus(context.Background(), Artifact{Content: "no_changes"})
	if err == nil {
		t.Fatal("未知结论应当被拒绝")
	}
	err = store.UpsertStatus(context.Background(), Artifact{Content: ""})
	if err == nil {
		t.Fatal("空结论应当被拒绝")
	}
}
