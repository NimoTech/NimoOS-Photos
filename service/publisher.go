package service

import (
	"context"
	"fmt"

	"github.com/NimoTech/NimoOS-Common/external"
)

// NewMessageBusPublisher 返回一个 TaskPublisher，把 Task 通过 NimoOS-Common
// 的 PublishEventInSocket 发到 MessageBus。
//
// 序列化策略：把 Task 的每个字段都转成字符串放到 Properties 中。
// 前端 photosTaskBusAdapter.unwrapTaskBusPayload 负责把 number 字段反转回去。
func NewMessageBusPublisher(parentCtx context.Context) TaskPublisher {
	return func(t Task) {
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
		if t.Error != "" {
			props["error"] = t.Error
		}
		// SourceID 与 Name 拼出 socket.io 事件名 "nimoos.photos.task.progress"。
		// NimoOS-MessageBus 的 SocketIOService.Publish 直接 BroadcastToRoom
		// 用 event.Name 作为事件名，所以 Name 取整串。
		_, _ = external.PublishEventInSocket(parentCtx,
			"nimoos.photos", "nimoos.photos.task.progress", props)
	}
}
