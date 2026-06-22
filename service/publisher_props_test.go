package service

import (
	"testing"
	"time"
)

// taskToProps 必须把 added(本次新增人脸数)带进 Properties——否则前端永远收不到、
// 「新识别 N 个人脸」提示永不弹出(曾经的回归)。
func TestTaskToProps_CarriesAdded(t *testing.T) {
	props := taskToProps(Task{
		ID: "face_1", Type: "face", Label: "识别人物",
		Current: 90, Total: 90, Added: 90, Progress: 1, Status: "done",
		StartedAt: time.Unix(0, 0),
	})
	if props["added"] != "90" {
		t.Fatalf("added 应为 \"90\"，实际 %q", props["added"])
	}
	if props["status"] != "done" || props["current"] != "90" {
		t.Fatalf("基础字段缺失: %#v", props)
	}
}

// added==0 时不应出现在 Properties(与 >0 才发的约定一致)。
func TestTaskToProps_OmitsZeroAdded(t *testing.T) {
	props := taskToProps(Task{
		ID: "face_1", Type: "face", Added: 0, Status: "done", StartedAt: time.Unix(0, 0),
	})
	if _, ok := props["added"]; ok {
		t.Fatalf("added==0 时不应出现在 Properties: %#v", props)
	}
}
