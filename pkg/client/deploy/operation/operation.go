package operation

import (
	"context"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
	"github.com/psviderski/uncloud/internal/machine/api/pb"
	"github.com/psviderski/uncloud/pkg/api"
)

// Operation represents a single atomic operation in a deployment process.
// Operations can be composed to form complex deployment strategies.
type Operation interface {
	// Execute performs the operation using the provided client.
	Execute(ctx context.Context, cli Client) error
	// Format returns a human-readable representation of the operation.
	Format() string
	String() string
}

// Client defines the interface required to execute deployment operations.
type Client interface {
	api.ContainerClient
	api.VolumeClient
	CreateContainerOnMachine(
		ctx context.Context, serviceID string, spec api.ServiceSpec, machine *pb.MachineInfo,
	) (container.CreateResponse, string, error)
	StartContainerOnMachine(ctx context.Context, machine *pb.MachineInfo, containerID, containerName string) error
	StopContainerOnMachine(
		ctx context.Context, machine *pb.MachineInfo, containerID, containerName string, opts container.StopOptions,
	) error
	RemoveContainerOnMachine(
		ctx context.Context, machine *pb.MachineInfo, containerID, containerName string, opts container.RemoveOptions,
	) error
	WaitContainerHealthyOnMachine(
		ctx context.Context, machine *pb.MachineInfo, containerID, containerName string, opts api.WaitContainerHealthyOptions,
	) error
	CreateVolumeOnMachine(ctx context.Context, machine *pb.MachineInfo, opts volume.CreateOptions) (api.MachineVolume, error)
}
