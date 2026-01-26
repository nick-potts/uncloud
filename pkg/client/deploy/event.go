package deploy

import "time"

// State represents the lifecycle state of a span.
type State string

const (
	StatePending State = "pending" // Waiting to start
	StateRunning State = "running" // In progress
	StateSuccess State = "success" // Completed successfully
	StateFailed  State = "failed"  // Failed with error
)

// Phase represents the deployment strategy phase.
type Phase string

const (
	PhaseDeploying   Phase = "deploying"    // Creating new containers
	PhaseCommitting  Phase = "committing"   // Removing old after new healthy
	PhaseRollingBack Phase = "rolling_back" // Reverting to previous
)

// Counts aggregates state counts for progress display.
type Counts struct {
	Pending int
	Running int
	Success int
	Failed  int
}

// Total returns the total number of spans.
func (c *Counts) Total() int {
	return c.Pending + c.Running + c.Success + c.Failed
}

// Complete returns the number of completed spans (success + failed).
func (c *Counts) Complete() int {
	return c.Success + c.Failed
}

// Span represents a unit of work in the deployment hierarchy.
// Observers should use Display fields for rendering without interpreting other fields.
type Span struct {
	ID       string
	ParentID string // Empty for root spans

	Phase     Phase
	State     State
	StartTime time.Time
	EndTime   time.Time
	Error     error

	// Counts aggregates descendant state counts (excludes self).
	Counts Counts

	// Progress tracks completion for operations like image pulls.
	// Only set for spans that have measurable progress.
	Progress *Progress

	// Display holds pre-computed text for rendering.
	// Observers should use these fields directly.
	Display Display
}

// Display holds pre-computed display text for observers.
// This allows observers to render spans without understanding span semantics.
type Display struct {
	// ID is the human-readable identifier for progress display.
	// Examples: "myservice", "Container abc123 on machine1", "Volume data on machine1"
	ID string

	// ParentID is the parent's display ID (not span UUID).
	// Empty for root-level spans in progress display.
	ParentID string

	// Text is the current status text (e.g., "Creating", "Pulling", "Waiting on db").
	Text string

	// EndText is the status text to show when the span completes.
	// For success: "Deployed", "Created", etc. For failure: error message.
	EndText string
}

// Progress tracks completion for operations with measurable progress.
type Progress struct {
	Current int64  // Current bytes or units completed
	Total   int64  // Total bytes or units
	Percent int    // Completion percentage (0-100)
	Text    string // Status text (e.g., "Downloading", "Extracting")
}

// IsRunning returns true if the span is currently running.
func (s *Span) IsRunning() bool {
	return s.State == StateRunning
}

// IsComplete returns true if the span has finished (success or failed).
func (s *Span) IsComplete() bool {
	return s.State == StateSuccess || s.State == StateFailed
}

// Duration returns how long the span has been running or took to complete.
func (s *Span) Duration() time.Duration {
	if s.EndTime.IsZero() {
		return time.Since(s.StartTime)
	}
	return s.EndTime.Sub(s.StartTime)
}

// SpanInfo provides display metadata for an operation.
// Operations implement this to describe how they should appear in progress output.
type SpanInfo struct {
	// DisplayID is the human-readable identifier shown in progress output.
	// Examples: "Container abc123 on machine1", "Volume data on machine1"
	DisplayID string

	// IsTopLevel indicates this span should appear at the root level in progress display,
	// without visual nesting under a parent even if ParentID is set internally.
	IsTopLevel bool

	// RunningText is the status shown while the operation is in progress.
	// Examples: "Creating", "Stopping", "Pulling"
	RunningText string

	// DoneText is the status shown when the operation completes successfully.
	// Examples: "Created", "Removed", "Pulled"
	DoneText string
}

// SpanInfoProvider is implemented by operations that can provide display metadata.
// This allows the executor to get display info without type-switching on operation types.
type SpanInfoProvider interface {
	// SpanInfo returns display metadata for this operation.
	// machineNames maps machine IDs to display names for formatting.
	SpanInfo(machineNames map[string]string) SpanInfo
}
