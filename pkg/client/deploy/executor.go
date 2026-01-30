package deploy

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/psviderski/uncloud/pkg/api"
)

// Executor executes deployment operations and emits progress events to observers.
type Executor struct {
	cli      Client
	observer Observer

	// machineNames caches machine ID to name mappings for display purposes.
	machineNames   map[string]string
	machineNamesMu sync.RWMutex
}

// NewExecutor creates a new Executor with the given client and observer.
func NewExecutor(cli Client, observer Observer) *Executor {
	if observer == nil {
		observer = NoopObserver{}
	}
	return &Executor{
		cli:          cli,
		observer:     observer,
		machineNames: make(map[string]string),
	}
}

// ExecutePlan executes a service deployment plan.
func (e *Executor) ExecutePlan(ctx context.Context, plan *Plan) error {
	e.prefetchMachineNames(ctx, plan.Operations)

	serviceID := plan.ServiceID
	serviceName := plan.ServiceName
	completed := 0
	total := len(plan.Operations)
	startTime := time.Now()

	e.emit(Event{
		ID:        serviceName,
		Status:    StatusRunning,
		Text:      "Deploying",
		Timestamp: startTime,
		Details: ServiceDetails{
			ServiceID:   serviceID,
			ServiceName: serviceName,
			Progress:    &Progress{Current: 0, Total: int64(total), Percent: 0},
		},
	})

	for _, op := range plan.Operations {
		if err := e.executeOperation(ctx, serviceName, op); err != nil {
			e.emit(Event{
				ID:        serviceName,
				Status:    StatusError,
				Text:      err.Error(),
				Timestamp: time.Now(),
				Duration:  time.Since(startTime),
				Details: ServiceDetails{
					ServiceID:   serviceID,
					ServiceName: serviceName,
					Progress:    &Progress{Current: int64(completed), Total: int64(total), Percent: completed * 100 / total},
				},
			})
			return err
		}
		completed++
		percent := 0
		if total > 0 {
			percent = completed * 100 / total
		}
		e.emit(Event{
			ID:        serviceName,
			Status:    StatusRunning,
			Text:      fmt.Sprintf("Deploying (%d/%d)", completed, total),
			Timestamp: time.Now(),
			Details: ServiceDetails{
				ServiceID:   serviceID,
				ServiceName: serviceName,
				Progress:    &Progress{Current: int64(completed), Total: int64(total), Percent: percent},
			},
		})
	}

	e.emit(Event{
		ID:        serviceName,
		Status:    StatusDone,
		Text:      fmt.Sprintf("Deployed (%d/%d)", completed, total),
		Timestamp: time.Now(),
		Duration:  time.Since(startTime),
		Details: ServiceDetails{
			ServiceID:   serviceID,
			ServiceName: serviceName,
			Progress:    &Progress{Current: int64(completed), Total: int64(total), Percent: 100},
		},
	})
	return nil
}

// ExecuteSequence executes a sequence of operations (compose deployment).
func (e *Executor) ExecuteSequence(ctx context.Context, seq *SequenceOperation) error {
	for _, op := range seq.Operations {
		switch o := op.(type) {
		case *Plan:
			if err := e.ExecutePlan(ctx, o); err != nil {
				return err
			}
		case *CreateVolumeOperation:
			if err := e.executeVolumeOperation(ctx, o); err != nil {
				return err
			}
		default:
			if err := op.Execute(ctx, e.cli); err != nil {
				return err
			}
		}
	}
	return nil
}

// executeOperation executes a single operation and emits progress events.
func (e *Executor) executeOperation(ctx context.Context, parentID string, op Operation) error {
	machineNames := e.getMachineNames()
	info := e.spanInfoFor(op, parentID)
	details := e.eventDetailsFor(op, machineNames)
	startTime := time.Now()

	e.emit(Event{
		ID:        info.ID,
		ParentID:  info.ParentID,
		Status:    StatusRunning,
		Text:      info.RunningText,
		Timestamp: startTime,
		Details:   details,
	})

	// Set up image pull callback for RunContainerOperation.
	if runOp, ok := op.(*RunContainerOperation); ok {
		runOp.CreateOpts.OnPullProgress = e.makeImagePullCallback(info.ID, runOp.Spec.Container.Image, runOp.MachineID)
	}

	err := op.Execute(ctx, e.cli)
	if err != nil {
		e.emit(Event{
			ID:        info.ID,
			ParentID:  info.ParentID,
			Status:    StatusError,
			Text:      err.Error(),
			Timestamp: time.Now(),
			Duration:  time.Since(startTime),
			Details:   details,
		})
		return err
	}

	e.emit(Event{
		ID:        info.ID,
		ParentID:  info.ParentID,
		Status:    StatusDone,
		Text:      info.DoneText,
		Timestamp: time.Now(),
		Duration:  time.Since(startTime),
		Details:   details,
	})
	return nil
}

// executeVolumeOperation executes a volume creation with progress events.
func (e *Executor) executeVolumeOperation(ctx context.Context, op *CreateVolumeOperation) error {
	machineNames := e.getMachineNames()
	info := op.SpanInfo(machineNames)
	details := op.EventDetails(machineNames)
	startTime := time.Now()

	e.emit(Event{
		ID:        info.ID,
		Status:    StatusRunning,
		Text:      info.RunningText,
		Timestamp: startTime,
		Details:   details,
	})

	err := op.Execute(ctx, e.cli)
	if err != nil {
		e.emit(Event{
			ID:        info.ID,
			Status:    StatusError,
			Text:      err.Error(),
			Timestamp: time.Now(),
			Duration:  time.Since(startTime),
			Details:   details,
		})
		return err
	}

	e.emit(Event{
		ID:        info.ID,
		Status:    StatusDone,
		Text:      info.DoneText,
		Timestamp: time.Now(),
		Duration:  time.Since(startTime),
		Details:   details,
	})
	return nil
}

// spanInfoFor returns SpanInfo for an operation, using the operation's method if available.
func (e *Executor) spanInfoFor(op Operation, parentID string) SpanInfo {
	if provider, ok := op.(SpanInfoProvider); ok {
		info := provider.SpanInfo(e.getMachineNames())
		if info.ParentID == "" {
			info.ParentID = parentID
		}
		return info
	}
	return SpanInfo{
		ID:          op.String(),
		ParentID:    parentID,
		RunningText: "Running",
		DoneText:    "Done",
	}
}

// eventDetailsFor returns Details for an operation, using the operation's method if available.
func (e *Executor) eventDetailsFor(op Operation, machineNames map[string]string) Details {
	if provider, ok := op.(EventDetailsProvider); ok {
		return provider.EventDetails(machineNames)
	}
	return nil
}

// makeImagePullCallback creates a callback that emits image pull progress events.
func (e *Executor) makeImagePullCallback(parentID, image, machineID string) api.ImagePullCallback {
	var (
		eventID   string
		startTime time.Time
		once      sync.Once
	)
	machineName := e.machineName(machineID)

	return func(p api.ImagePullProgress) {
		once.Do(func() {
			eventID = fmt.Sprintf("Image %s on %s", image, machineName)
			startTime = time.Now()
		})

		details := ImagePullDetails{
			MachineID:   machineID,
			MachineName: machineName,
			Image:       image,
		}

		if p.Total > 0 {
			details.Progress = &Progress{
				Current: p.Current,
				Total:   p.Total,
				Percent: p.Percent,
			}
		}

		if p.Error != nil {
			e.emit(Event{
				ID:        eventID,
				ParentID:  parentID,
				Status:    StatusError,
				Text:      p.Error.Error(),
				Timestamp: time.Now(),
				Duration:  time.Since(startTime),
				Details:   details,
			})
			return
		}

		if p.Done {
			e.emit(Event{
				ID:        eventID,
				ParentID:  parentID,
				Status:    StatusDone,
				Text:      "Pulled",
				Timestamp: time.Now(),
				Duration:  time.Since(startTime),
				Details:   details,
			})
			return
		}

		e.emit(Event{
			ID:        eventID,
			ParentID:  parentID,
			Status:    StatusRunning,
			Text:      p.Status,
			Timestamp: time.Now(),
			Details:   details,
		})
	}
}

// prefetchMachineNames loads machine names into the cache for display purposes.
func (e *Executor) prefetchMachineNames(ctx context.Context, ops []Operation) {
	machineIDs := make(map[string]struct{})
	for _, op := range ops {
		switch o := op.(type) {
		case *RunContainerOperation:
			machineIDs[o.MachineID] = struct{}{}
		case *StopContainerOperation:
			machineIDs[o.MachineID] = struct{}{}
		case *RemoveContainerOperation:
			machineIDs[o.MachineID] = struct{}{}
		}
	}

	e.machineNamesMu.Lock()
	defer e.machineNamesMu.Unlock()

	for machineID := range machineIDs {
		if _, ok := e.machineNames[machineID]; ok {
			continue
		}
		machine, err := e.cli.InspectMachine(ctx, machineID)
		if err != nil {
			continue
		}
		if machine.Machine != nil {
			e.machineNames[machineID] = machine.Machine.Name
		}
	}
}

// machineName returns the display name for a machine ID.
func (e *Executor) machineName(machineID string) string {
	e.machineNamesMu.RLock()
	name := e.machineNames[machineID]
	e.machineNamesMu.RUnlock()

	if name != "" {
		return name
	}
	if len(machineID) > 12 {
		return machineID[:12]
	}
	return machineID
}

// getMachineNames returns a copy of the machine names map.
func (e *Executor) getMachineNames() map[string]string {
	e.machineNamesMu.RLock()
	defer e.machineNamesMu.RUnlock()

	names := make(map[string]string, len(e.machineNames))
	for k, v := range e.machineNames {
		names[k] = v
	}
	return names
}

// emit sends an event to the observer.
func (e *Executor) emit(event Event) {
	e.observer.OnEvent(event)
}
