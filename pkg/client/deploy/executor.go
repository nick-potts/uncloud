package deploy

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/psviderski/uncloud/pkg/api"
)

// Executor executes deployment operations and emits span events to observers.
type Executor struct {
	cli      Client
	observer Observer

	// machineNames caches machine ID to name mappings for display purposes.
	machineNames map[string]string

	spans map[string]*spanNode
	mu    sync.Mutex
}

type spanNode struct {
	span      *Span
	parent    *spanNode
	children  []*spanNode
	lastState State // For incremental count updates
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
		spans:        make(map[string]*spanNode),
	}
}

// ExecutePlan executes a service deployment plan.
func (e *Executor) ExecutePlan(ctx context.Context, plan *Plan) error {
	// Prefetch machine names for display purposes.
	if err := e.prefetchMachineNames(ctx, plan); err != nil {
		// Non-fatal: we'll fall back to truncated IDs.
		_ = err
	}

	// Create service span.
	serviceSpanID := e.startServiceSpan(plan.ServiceName)

	// Execute operations.
	for _, op := range plan.Operations {
		if err := e.executeOperation(ctx, serviceSpanID, op); err != nil {
			e.endSpan(serviceSpanID, StateFailed, err)
			return err
		}
	}

	e.endSpan(serviceSpanID, StateSuccess, nil)
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

// prefetchMachineNames loads machine names into the cache for display purposes.
func (e *Executor) prefetchMachineNames(ctx context.Context, plan *Plan) error {
	machineIDs := make(map[string]struct{})
	for _, op := range plan.Operations {
		switch o := op.(type) {
		case *RunContainerOperation:
			machineIDs[o.MachineID] = struct{}{}
		case *StopContainerOperation:
			machineIDs[o.MachineID] = struct{}{}
		case *RemoveContainerOperation:
			machineIDs[o.MachineID] = struct{}{}
		}
	}

	for machineID := range machineIDs {
		if _, ok := e.machineNames[machineID]; ok {
			continue
		}
		machine, err := e.cli.InspectMachine(ctx, machineID)
		if err != nil {
			continue // Non-fatal, will use truncated ID.
		}
		if machine.Machine != nil {
			e.machineNames[machineID] = machine.Machine.Name
		}
	}
	return nil
}

// executeVolumeOperation executes a volume creation as a span.
func (e *Executor) executeVolumeOperation(ctx context.Context, op *CreateVolumeOperation) error {
	info := op.SpanInfo(e.machineNames)
	spanID := e.startOperationSpan("", info)

	err := op.Execute(ctx, e.cli)
	if err != nil {
		e.endSpan(spanID, StateFailed, err)
		return err
	}

	e.endSpan(spanID, StateSuccess, nil)
	return nil
}

// startServiceSpan creates a service-level span that groups container operations.
func (e *Executor) startServiceSpan(serviceName string) string {
	e.mu.Lock()
	defer e.mu.Unlock()

	id := uuid.New().String()
	span := &Span{
		ID:        id,
		State:     StateRunning,
		Phase:     PhaseDeploying,
		StartTime: time.Now(),
		Display: Display{
			ID:   serviceName,
			Text: "Deploying",
		},
	}

	node := &spanNode{span: span, lastState: StateRunning}
	e.spans[id] = node

	e.observer.OnSpanStart(span)
	return id
}

// startOperationSpan creates a span for an operation using its SpanInfo.
func (e *Executor) startOperationSpan(parentID string, info SpanInfo) string {
	e.mu.Lock()
	defer e.mu.Unlock()

	id := uuid.New().String()
	span := &Span{
		ID:        id,
		ParentID:  parentID,
		State:     StateRunning,
		StartTime: time.Now(),
		Display: Display{
			ID:      info.DisplayID,
			Text:    info.RunningText,
			EndText: info.DoneText,
		},
	}

	// Set display parent based on whether this is a top-level span.
	if !info.IsTopLevel && parentID != "" {
		if parent, ok := e.spans[parentID]; ok {
			span.Display.ParentID = parent.span.Display.ID
		}
	}

	node := &spanNode{span: span, lastState: StateRunning}
	e.spans[id] = node

	// Link to parent for count propagation.
	if parentID != "" {
		if parent, ok := e.spans[parentID]; ok {
			node.parent = parent
			parent.children = append(parent.children, node)
			// Increment parent's running count.
			e.propagateCountChange(node, "", StateRunning)
		}
	}

	e.observer.OnSpanStart(span)
	return id
}

// updateSpanProgress updates the progress of a span without changing its state.
func (e *Executor) updateSpanProgress(spanID string, progress *Progress) {
	e.mu.Lock()
	defer e.mu.Unlock()

	node, ok := e.spans[spanID]
	if !ok {
		return
	}

	node.span.Progress = progress
	if progress.Text != "" {
		node.span.Display.Text = progress.Text
	}
	e.observer.OnSpanUpdate(node.span)
}

// endSpan completes a span with the given state and optional error.
func (e *Executor) endSpan(spanID string, state State, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	node, ok := e.spans[spanID]
	if !ok {
		return
	}

	oldState := node.lastState
	node.span.State = state
	node.span.EndTime = time.Now()
	node.span.Error = err
	node.lastState = state

	// Update end text.
	if state == StateFailed && err != nil {
		node.span.Display.EndText = err.Error()
	} else if node.span.Display.EndText == "" {
		// Fallback if not set.
		node.span.Display.EndText = string(state)
	}

	// For service spans, show completion counts.
	if node.span.Counts.Total() > 0 {
		node.span.Display.EndText = fmt.Sprintf("Deployed (%d/%d)",
			node.span.Counts.Complete(), node.span.Counts.Total())
	}

	if oldState != state {
		e.propagateCountChange(node, oldState, state)
	}

	e.observer.OnSpanEnd(node.span)
}

// propagateCountChange updates ancestor Counts when a span state changes.
func (e *Executor) propagateCountChange(node *spanNode, oldState, newState State) {
	for p := node.parent; p != nil; p = p.parent {
		// Decrement old state count.
		switch oldState {
		case StatePending:
			p.span.Counts.Pending--
		case StateRunning:
			p.span.Counts.Running--
		case StateSuccess:
			p.span.Counts.Success--
		case StateFailed:
			p.span.Counts.Failed--
		}
		// Increment new state count.
		switch newState {
		case StatePending:
			p.span.Counts.Pending++
		case StateRunning:
			p.span.Counts.Running++
		case StateSuccess:
			p.span.Counts.Success++
		case StateFailed:
			p.span.Counts.Failed++
		}
		// Update display text with progress counts.
		p.span.Display.Text = fmt.Sprintf("Deploying (%d/%d)",
			p.span.Counts.Complete(), p.span.Counts.Total())

		e.observer.OnSpanUpdate(p.span)
	}
}

// executeOperation executes a single operation as a child span.
func (e *Executor) executeOperation(ctx context.Context, parentSpanID string, op Operation) error {
	// Get span info from the operation if it implements SpanInfoProvider.
	var info SpanInfo
	if provider, ok := op.(SpanInfoProvider); ok {
		info = provider.SpanInfo(e.machineNames)
	} else {
		// Fallback for operations that don't implement SpanInfoProvider.
		info = SpanInfo{
			DisplayID:   op.String(),
			RunningText: "Running",
			DoneText:    "Done",
		}
	}

	spanID := e.startOperationSpan(parentSpanID, info)

	// Set up image pull callback for RunContainerOperation.
	if runOp, ok := op.(*RunContainerOperation); ok {
		runOp.CreateOpts.OnPullProgress = e.makeImagePullCallback(spanID, runOp.Spec.Container.Image, runOp.MachineID)
	}

	// Execute the operation.
	err := op.Execute(ctx, e.cli)
	if err != nil {
		e.endSpan(spanID, StateFailed, err)
		return err
	}

	e.endSpan(spanID, StateSuccess, nil)
	return nil
}

// makeImagePullCallback creates a callback that emits image pull progress as a child span.
func (e *Executor) makeImagePullCallback(parentSpanID, image, machineID string) api.ImagePullCallback {
	var imageSpanID string
	var once sync.Once

	return func(p api.ImagePullProgress) {
		// Create image span on first progress callback.
		once.Do(func() {
			machine := e.machineNames[machineID]
			if machine == "" {
				machine = machineID[:12]
				if len(machineID) < 12 {
					machine = machineID
				}
			}
			info := SpanInfo{
				DisplayID:   fmt.Sprintf("Image %s on %s", image, machine),
				RunningText: "Pulling",
				DoneText:    "Pulled",
			}
			imageSpanID = e.startOperationSpan(parentSpanID, info)
		})

		if imageSpanID == "" {
			return
		}

		if p.Error != nil {
			e.endSpan(imageSpanID, StateFailed, p.Error)
			return
		}

		// Update span with progress.
		e.updateSpanProgress(imageSpanID, &Progress{
			Current: p.Current,
			Total:   p.Total,
			Percent: p.Percent,
			Text:    p.Status,
		})

		// Complete span when image is fully pulled.
		if p.Done {
			e.endSpan(imageSpanID, StateSuccess, nil)
		}
	}
}
