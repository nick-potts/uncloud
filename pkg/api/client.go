package api

import (
	"context"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
	"github.com/psviderski/uncloud/internal/machine/api/pb"
)

type Client interface {
	ContainerClient
	DNSClient
	ImageClient
	MachineClient
	ServiceClient
	VolumeClient
}

// ImagePullProgress contains progress information for an image pull operation.
type ImagePullProgress struct {
	// LayerID is the ID of the layer being processed (empty for overall status).
	LayerID string
	// Status describes the current operation (e.g., "Downloading", "Extracting").
	Status string
	// Current is the number of bytes completed for this layer.
	Current int64
	// Total is the total number of bytes for this layer.
	Total int64
	// Percent is the completion percentage (0-100) for this layer.
	Percent int
	// Done indicates this layer has completed.
	Done bool
	// Error is set if the pull failed.
	Error error
}

// ImagePullCallback is called with progress updates during image pulling.
type ImagePullCallback func(ImagePullProgress)

// CreateContainerOptions contains options for creating a container.
type CreateContainerOptions struct {
	// OnPullProgress is called with progress updates if an image needs to be pulled.
	OnPullProgress ImagePullCallback
}

type ContainerClient interface {
	CreateContainer(
		ctx context.Context, serviceID string, spec ServiceSpec, machineID string, opts CreateContainerOptions,
	) (container.CreateResponse, error)
	InspectContainer(ctx context.Context, serviceNameOrID, containerNameOrID string) (MachineServiceContainer, error)
	RemoveContainer(ctx context.Context, serviceNameOrID, containerNameOrID string, opts container.RemoveOptions) error
	StartContainer(ctx context.Context, serviceNameOrID, containerNameOrID string) error
	StopContainer(ctx context.Context, serviceNameOrID, containerNameOrID string, opts container.StopOptions) error
	ExecContainer(ctx context.Context, serviceNameOrID, containerNameOrID string, config ExecOptions) (int, error)
}

type DNSClient interface {
	GetDomain(ctx context.Context) (string, error)
}

type ImageClient interface {
	InspectImage(ctx context.Context, id string) ([]MachineImage, error)
	InspectRemoteImage(ctx context.Context, id string) ([]MachineRemoteImage, error)
}

type MachineClient interface {
	InspectMachine(ctx context.Context, id string) (*pb.MachineMember, error)
	ListMachines(ctx context.Context, filter *MachineFilter) (MachineMembersList, error)
	UpdateMachine(ctx context.Context, req *pb.UpdateMachineRequest) (*pb.MachineInfo, error)
	RenameMachine(ctx context.Context, nameOrID, newName string) (*pb.MachineInfo, error)
}

type ServiceClient interface {
	RunService(ctx context.Context, spec ServiceSpec) (RunServiceResponse, error)
	InspectService(ctx context.Context, id string) (Service, error)
	RemoveService(ctx context.Context, id string) error
	StopService(ctx context.Context, id string, opts container.StopOptions) error
	StartService(ctx context.Context, id string) error
}

type VolumeClient interface {
	CreateVolume(ctx context.Context, machineNameOrID string, opts volume.CreateOptions) (MachineVolume, error)
	ListVolumes(ctx context.Context, filter *VolumeFilter) ([]MachineVolume, error)
	RemoveVolume(ctx context.Context, machineNameOrID, volumeName string, force bool) error
}
