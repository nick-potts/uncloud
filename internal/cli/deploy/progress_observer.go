package deploy

import (
	"github.com/docker/compose/v2/pkg/progress"
	"github.com/psviderski/uncloud/pkg/client/deploy"
)

// ProgressObserver adapts span events to docker compose progress events.
// It simply maps Span.Display fields to progress.Event fields.
type ProgressObserver struct {
	writer progress.Writer
}

// NewProgressObserver creates a new ProgressObserver.
func NewProgressObserver(w progress.Writer) *ProgressObserver {
	return &ProgressObserver{
		writer: w,
	}
}

func (p *ProgressObserver) OnSpanStart(s *deploy.Span) {
	p.writer.Event(progress.Event{
		ID:         s.Display.ID,
		ParentID:   s.Display.ParentID,
		Status:     progress.Working,
		StatusText: s.Display.Text,
	})
}

func (p *ProgressObserver) OnSpanUpdate(s *deploy.Span) {
	p.writer.Event(progress.Event{
		ID:         s.Display.ID,
		ParentID:   s.Display.ParentID,
		Status:     progressStatus(s.State),
		StatusText: s.Display.Text,
	})
}

func (p *ProgressObserver) OnSpanEnd(s *deploy.Span) {
	p.writer.Event(progress.Event{
		ID:         s.Display.ID,
		ParentID:   s.Display.ParentID,
		Status:     progressStatus(s.State),
		StatusText: s.Display.EndText,
	})
}

// progressStatus maps span state to progress event status.
func progressStatus(state deploy.State) progress.EventStatus {
	switch state {
	case deploy.StateSuccess:
		return progress.Done
	case deploy.StateFailed:
		return progress.Error
	default:
		return progress.Working
	}
}
