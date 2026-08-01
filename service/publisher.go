package service

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/NimoTech/NimoOS-Common/external"
)

// NewMessageBusPublisher returns a TaskPublisher that sends a Task to
// MessageBus via NimoOS-Common's PublishEventInSocket.
//
// Serialization strategy: every Task field is converted to a string and put
// into Properties. The frontend's photosTaskBusAdapter.unwrapTaskBusPayload
// converts number fields back.
func NewMessageBusPublisher(parentCtx context.Context) TaskPublisher {
	return func(t Task) {
		// SourceID and Name together form the socket.io event name
		// "nimoos.photos.task.progress". NimoOS-MessageBus's
		// SocketIOService.Publish calls BroadcastToRoom directly using
		// event.Name as the event name, so Name is passed as the full string.
		_, _ = external.PublishEventInSocket(parentCtx,
			"nimoos.photos", "nimoos.photos.task.progress", taskToProps(t))
	}
}

// taskToProps converts every Task field to a string and puts it into
// Properties (MessageBus only accepts map[string]string). The frontend's
// photosTaskBusAdapter.unwrapTaskBusPayload converts number fields back.
// Extracted into a pure function for unit testing, to avoid regressions
// where a new field gets forgotten in Properties.
func taskToProps(t Task) map[string]string {
	props := map[string]string{
		"id":         t.ID,
		"type":       t.Type,
		"label":      t.Label,
		"current":    fmt.Sprintf("%d", t.Current),
		"total":      fmt.Sprintf("%d", t.Total),
		"progress":   fmt.Sprintf("%.4f", t.Progress),
		"status":     t.Status,
		"started_at": t.StartedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	if t.ETASeconds > 0 {
		props["eta_seconds"] = fmt.Sprintf("%d", t.ETASeconds)
	}
	if t.Added > 0 {
		// The "count added this run" carried in the terminal state (used by
		// face clustering): only sent when >0, so the frontend can pop
		// "N new faces recognized".
		props["added"] = fmt.Sprintf("%d", t.Added)
	}
	if t.Error != "" {
		props["error"] = t.Error
	}
	if t.ErrorKey != "" {
		props["errorKey"] = t.ErrorKey
	}
	if len(t.ErrorParams) > 0 {
		// MessageBus Properties only accepts map[string]string; errorParams
		// is itself a map[string]string, so it's serialized to a JSON string
		// and stuffed in, then deserialized back on the frontend.
		if b, err := json.Marshal(t.ErrorParams); err == nil {
			props["errorParams"] = string(b)
		}
	}
	return props
}
