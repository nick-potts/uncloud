package deploy

import "time"

// Event represents a deployment progress event sent to observers.
// Events contain both display-ready fields (for simple observers like CLI)
// and structured data (for rich observers like web UI).
type Event struct {
	// ID is the human-readable identifier for progress display.
	// Examples: "myservice", "Container on machine1", "Volume data on machine1"
	ID string

	// ParentID is the display ID of the parent event.
	// Empty for root-level events.
	ParentID string

	// Status describes the current state.
	Status EventStatus

	// Text is the status text (e.g., "Creating", "Deployed", error message).
	Text string

	// Timestamp is when this event was emitted.
	Timestamp time.Time

	// Duration is how long the operation took. Only set when Status is Done or Error.
	Duration time.Duration

	// Details contains type-specific structured data for rich observers.
	// Simple observers can ignore this field.
	// Use type assertion to access specific details (e.g., ContainerDetails, VolumeDetails).
	Details Details
}

// EventStatus represents the lifecycle state of an operation.
type EventStatus int

const (
	StatusRunning EventStatus = iota
	StatusDone
	StatusError
)

// Details is implemented by all event detail types.
// Rich observers can type-assert to access specific fields.
type Details interface {
	// eventDetails is a marker method to identify detail types.
	eventDetails()
}

// Progress represents completion progress for long-running operations.
type Progress struct {
	// Current is the number of units completed.
	Current int64
	// Total is the total number of units. Zero if unknown.
	Total int64
	// Percent is the completion percentage (0-100). -1 if unknown.
	Percent int
}

// ServiceDetails contains structured data for service-level events.
type ServiceDetails struct {
	ServiceID   string
	ServiceName string
	// Progress tracks operation completion (e.g., 3/5 containers deployed).
	Progress *Progress
}

func (ServiceDetails) eventDetails() {}

// ContainerAction describes what operation is being performed on a container.
type ContainerAction string

const (
	ContainerActionCreate ContainerAction = "create"
	ContainerActionStart  ContainerAction = "start"
	ContainerActionStop   ContainerAction = "stop"
	ContainerActionRemove ContainerAction = "remove"
)

// ContainerDetails contains structured data for container operation events.
type ContainerDetails struct {
	MachineID   string
	MachineName string
	ContainerID string
	Image       string
	Action      ContainerAction
}

func (ContainerDetails) eventDetails() {}

// VolumeDetails contains structured data for volume operation events.
type VolumeDetails struct {
	MachineID   string
	MachineName string
	VolumeName  string
}

func (VolumeDetails) eventDetails() {}

// ImagePullDetails contains structured data for image pull events.
type ImagePullDetails struct {
	MachineID   string
	MachineName string
	Image       string
	// Progress tracks pull completion. Nil if not yet known.
	Progress *Progress
}

func (ImagePullDetails) eventDetails() {}

// SpanInfo provides display metadata for an operation.
// Operations implement SpanInfoProvider to describe how they should appear in progress output.
type SpanInfo struct {
	// ID is the human-readable identifier shown in progress output.
	// Examples: "Container on machine1", "Volume data on machine1"
	ID string

	// ParentID is the parent's display ID. Empty for top-level items.
	ParentID string

	// RunningText is the status shown while the operation is in progress.
	// Examples: "Creating", "Stopping", "Pulling"
	RunningText string

	// DoneText is the status shown when the operation completes successfully.
	// Examples: "Created", "Removed", "Pulled"
	DoneText string
}

// SpanInfoProvider is implemented by operations that can provide display metadata.
type SpanInfoProvider interface {
	// SpanInfo returns display metadata for this operation.
	// machineNames maps machine IDs to display names for formatting.
	SpanInfo(machineNames map[string]string) SpanInfo
}

// EventDetailsProvider is implemented by operations that can provide structured event data.
type EventDetailsProvider interface {
	// EventDetails returns structured data for this operation.
	// machineNames maps machine IDs to display names.
	EventDetails(machineNames map[string]string) Details
}
