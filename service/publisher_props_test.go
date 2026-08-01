package service

import (
	"encoding/json"
	"testing"
	"time"
)

// taskToProps must carry added (the number of faces newly added this run)
// into Properties — otherwise the frontend never receives it and the
// "N new faces recognized" hint never pops (a past regression).
func TestTaskToProps_CarriesAdded(t *testing.T) {
	props := taskToProps(Task{
		ID: "face_1", Type: "face", Label: "Recognizing people",
		Current: 90, Total: 90, Added: 90, Progress: 1, Status: "done",
		StartedAt: time.Unix(0, 0),
	})
	if props["added"] != "90" {
		t.Fatalf("added should be \"90\", got %q", props["added"])
	}
	if props["status"] != "done" || props["current"] != "90" {
		t.Fatalf("basic fields missing: %#v", props)
	}
}

// added==0 should not appear in Properties (consistent with the "only sent when >0" convention).
func TestTaskToProps_OmitsZeroAdded(t *testing.T) {
	props := taskToProps(Task{
		ID: "face_1", Type: "face", Added: 0, Status: "done", StartedAt: time.Unix(0, 0),
	})
	if _, ok := props["added"]; ok {
		t.Fatalf("added==0 should not appear in Properties: %#v", props)
	}
}

// taskToProps must carry the structured i18n error (errorKey + errorParams)
// into Properties — otherwise the frontend can't get the key/params and has
// to fall back to displaying the old plain-sentence Error, making i18n
// pointless. errorParams is serialized to a JSON string (Properties only
// accepts map[string]string).
func TestTaskToProps_CarriesErrorKeyAndParams(t *testing.T) {
	tk := Task{ID: "face_1", Type: "face", Status: "error", StartedAt: time.Unix(0, 0)}
	tk.SetError(TaskErrFaceClusterFailed, map[string]string{"detail": "boom"})
	props := taskToProps(tk)

	if props["errorKey"] != TaskErrFaceClusterFailed {
		t.Fatalf("errorKey should be the contract key, got %q", props["errorKey"])
	}
	if props["error"] != "Face clustering failed: boom" {
		t.Fatalf("error should be the English fallback, got %q", props["error"])
	}
	var params map[string]string
	if err := json.Unmarshal([]byte(props["errorParams"]), &params); err != nil {
		t.Fatalf("errorParams should be valid JSON: %v (%q)", err, props["errorParams"])
	}
	if params["detail"] != "boom" {
		t.Fatalf("errorParams.detail should be \"boom\", got %#v", params)
	}
}

// When there's no structured error (ErrorKey is empty), neither errorKey nor errorParams should appear in Properties.
func TestTaskToProps_OmitsEmptyErrorKey(t *testing.T) {
	props := taskToProps(Task{
		ID: "face_1", Type: "face", Status: "done", StartedAt: time.Unix(0, 0),
	})
	if _, ok := props["errorKey"]; ok {
		t.Fatalf("errorKey should not appear in Properties when empty: %#v", props)
	}
	if _, ok := props["errorParams"]; ok {
		t.Fatalf("errorParams should not appear in Properties when empty: %#v", props)
	}
}
