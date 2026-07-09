package service

import (
	"encoding/json"
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

// taskToProps 必须把结构化 i18n 错误(errorKey + errorParams)带进 Properties——
// 否则前端拿不到 key/参数，只能退回旧的整句 Error 展示，i18n 白做。
// errorParams 序列化成 JSON 字符串（Properties 只收 map[string]string）。
func TestTaskToProps_CarriesErrorKeyAndParams(t *testing.T) {
	tk := Task{ID: "face_1", Type: "face", Status: "error", StartedAt: time.Unix(0, 0)}
	tk.SetError(TaskErrFaceClusterFailed, map[string]string{"detail": "boom"})
	props := taskToProps(tk)

	if props["errorKey"] != TaskErrFaceClusterFailed {
		t.Fatalf("errorKey 应为契约 key，实际 %q", props["errorKey"])
	}
	if props["error"] != "Face clustering failed: boom" {
		t.Fatalf("error 应为英文 fallback，实际 %q", props["error"])
	}
	var params map[string]string
	if err := json.Unmarshal([]byte(props["errorParams"]), &params); err != nil {
		t.Fatalf("errorParams 应是合法 JSON: %v (%q)", err, props["errorParams"])
	}
	if params["detail"] != "boom" {
		t.Fatalf("errorParams.detail 应为 \"boom\"，实际 %#v", params)
	}
}

// 没有结构化错误(ErrorKey 为空)时,errorKey/errorParams 都不应出现在 Properties。
func TestTaskToProps_OmitsEmptyErrorKey(t *testing.T) {
	props := taskToProps(Task{
		ID: "face_1", Type: "face", Status: "done", StartedAt: time.Unix(0, 0),
	})
	if _, ok := props["errorKey"]; ok {
		t.Fatalf("errorKey 为空时不应出现在 Properties: %#v", props)
	}
	if _, ok := props["errorParams"]; ok {
		t.Fatalf("errorParams 为空时不应出现在 Properties: %#v", props)
	}
}
