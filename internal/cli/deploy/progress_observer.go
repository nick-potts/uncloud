package deploy

import (
	"github.com/docker/compose/v2/pkg/progress"
	"github.com/psviderski/uncloud/pkg/client/deploy"
)

// ProgressObserver adapts deploy events to docker compose progress events.
type ProgressObserver struct {
	writer progress.Writer
}

// NewProgressObserver creates a new ProgressObserver.
func NewProgressObserver(w progress.Writer) *ProgressObserver {
	return &ProgressObserver{writer: w}
}

// OnEvent converts a deploy event to a progress event.
func (p *ProgressObserver) OnEvent(e deploy.Event) {
	p.writer.Event(progress.Event{
		ID:         e.ID,
		ParentID:   e.ParentID,
		Status:     progressStatus(e.Status),
		StatusText: e.Text,
	})
}

func progressStatus(status deploy.EventStatus) progress.EventStatus {
	switch status {
	case deploy.StatusDone:
		return progress.Done
	case deploy.StatusError:
		return progress.Error
	default:
		return progress.Working
	}
}
