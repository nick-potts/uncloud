package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
	"github.com/psviderski/uncloud/internal/machine/api/pb"
	"github.com/psviderski/uncloud/pkg/api"
	"github.com/psviderski/uncloud/pkg/client/deploy/scheduler"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (cli *Client) RunService(ctx context.Context, spec api.ServiceSpec) (api.RunServiceResponse, error) {
	var resp api.RunServiceResponse

	if err := spec.Validate(); err != nil {
		return resp, fmt.Errorf("invalid service spec: %w", err)
	}

	var snapshot *ClusterSnapshot
	if spec.Name != "" {
		var err error
		snapshot, err = cli.InspectClusterSnapshot(ctx)
		if err != nil {
			return resp, fmt.Errorf("inspect cluster snapshot: %w", err)
		}
		if _, err = snapshot.Service(spec.Name); err == nil {
			return resp, fmt.Errorf("service with name '%s' already exists", spec.Name)
		}
		if !errors.Is(err, api.ErrNotFound) {
			return resp, fmt.Errorf("inspect service: %w", err)
		}
	}

	// Create missing named Docker volumes for the service.
	if len(spec.MountedDockerVolumes()) > 0 {
		state, err := scheduler.InspectClusterState(ctx, cli)
		if err != nil {
			return resp, fmt.Errorf("inspect cluster state: %w", err)
		}
		volumeScheduler, err := scheduler.NewVolumeScheduler(state, []api.ServiceSpec{spec})
		if err != nil {
			return resp, fmt.Errorf("init volume scheduler: %w", err)
		}

		scheduledVolumes, err := volumeScheduler.Schedule()
		if err != nil {
			return resp, fmt.Errorf("schedule volumes: %w", err)
		}

		if snapshot == nil {
			snapshot, err = cli.InspectClusterSnapshot(ctx)
			if err != nil {
				return resp, fmt.Errorf("inspect cluster snapshot: %w", err)
			}
		}

		// Create the missing volumes on the scheduled machines.
		for machineID, volumes := range scheduledVolumes {
			machine, err := snapshot.Machine(machineID)
			if err != nil {
				return resp, fmt.Errorf("resolve machine '%s': %w", machineID, err)
			}
			for _, v := range volumes {
				opts := volume.CreateOptions{
					Name: v.Name,
				}
				if v.VolumeOptions != nil {
					if v.VolumeOptions.Driver != nil {
						opts.Driver = v.VolumeOptions.Driver.Name
						opts.DriverOpts = v.VolumeOptions.Driver.Options
					}
					opts.Labels = v.VolumeOptions.Labels
				}

				if _, err = cli.CreateVolumeOnMachine(ctx, machine.Machine, opts); err != nil {
					return resp, fmt.Errorf("create volume '%s': %w", v.Name, err)
				}
			}
		}
	}

	deployment := cli.NewDeployment(spec, nil)
	if snapshot != nil && spec.Name != "" {
		deployment.UseResolvedState(nil, nil)
	}
	plan, err := deployment.Run(ctx)
	if err != nil {
		return resp, err
	}

	resp.ID = plan.ServiceID
	resp.Name = plan.ServiceName

	return resp, err
}

// InspectService returns detailed information about a service and its containers.
// The nameOrID parameter can be either a service name or ID.
func (cli *Client) InspectService(ctx context.Context, nameOrID string) (api.Service, error) {
	snapshot, err := cli.InspectClusterSnapshot(ctx)
	if err != nil {
		return api.Service{}, err
	}
	printSnapshotWarnings(snapshot.Warnings)
	return snapshot.Service(nameOrID)
}

// InspectServiceFromStore returns detailed information about a service and its containers from the distributed store.
// Due to eventual consistency of the store, the returned information may not reflect the most recent changes.
// The id parameter can be either a service ID or name.
func (cli *Client) InspectServiceFromStore(ctx context.Context, id string) (api.Service, error) {
	var svc api.Service

	resp, err := cli.MachineClient.InspectService(ctx, &pb.InspectServiceRequest{Id: id})
	if err != nil {
		if s, ok := status.FromError(err); ok {
			if s.Code() == codes.NotFound {
				return svc, api.ErrNotFound
			}
		}
		return svc, err
	}

	svc, err = api.ServiceFromProto(resp.Service)
	if err != nil {
		return svc, fmt.Errorf("from proto: %w", err)
	}
	return svc, nil
}

// RemoveService removes all containers on all machines that belong to the specified service.
// The id parameter can be either a service ID or name.
func (cli *Client) RemoveService(ctx context.Context, id string) error {
	snapshot, err := cli.InspectClusterSnapshot(ctx)
	if err != nil {
		return err
	}
	printSnapshotWarnings(snapshot.Warnings)

	svc, err := snapshot.Service(id)
	if err != nil {
		return err
	}

	wg := sync.WaitGroup{}
	errCh := make(chan error)

	// Remove all containers on all machines that belong to the service.
	for _, mc := range svc.Containers {
		machine, err := snapshot.Machine(mc.MachineID)
		if err != nil {
			return fmt.Errorf("resolve machine '%s': %w", mc.MachineID, err)
		}
		wg.Go(func() {
			err := cli.StopContainerOnMachine(ctx, machine.Machine, mc.Container.ID, mc.Container.Name, container.StopOptions{})
			if err != nil {
				errCh <- fmt.Errorf("stop container '%s': %w", mc.Container.ID, err)
				return
			}

			err = cli.RemoveContainerOnMachine(ctx, machine.Machine, mc.Container.ID, mc.Container.Name, container.RemoveOptions{
				// Remove anonymous volumes created by the container.
				RemoveVolumes: true,
			})
			if err != nil && !errors.Is(err, api.ErrNotFound) {
				errCh <- fmt.Errorf("remove container '%s': %w", mc.Container.ID, err)
			}
		})
	}

	go func() {
		wg.Wait()
		close(errCh)
	}()

	err = nil
	for e := range errCh {
		err = errors.Join(err, e)
	}
	return err
}

// StopService stops all containers on all machines that belong to the specified service.
// The id parameter can be either a service ID or name.
func (cli *Client) StopService(ctx context.Context, id string, opts container.StopOptions) error {
	snapshot, err := cli.InspectClusterSnapshot(ctx)
	if err != nil {
		return err
	}
	printSnapshotWarnings(snapshot.Warnings)

	svc, err := snapshot.Service(id)
	if err != nil {
		return err
	}

	wg := sync.WaitGroup{}
	errCh := make(chan error)

	// Stop all containers on all machines that belong to the service.
	for _, mc := range svc.Containers {
		machine, err := snapshot.Machine(mc.MachineID)
		if err != nil {
			return fmt.Errorf("resolve machine '%s': %w", mc.MachineID, err)
		}
		wg.Go(func() {
			err := cli.StopContainerOnMachine(ctx, machine.Machine, mc.Container.ID, mc.Container.Name, opts)
			if err != nil {
				errCh <- fmt.Errorf("stop container '%s': %w", mc.Container.ID, err)
			}
		})
	}

	go func() {
		wg.Wait()
		close(errCh)
	}()

	err = nil
	for e := range errCh {
		err = errors.Join(err, e)
	}
	return err
}

// StartService starts all containers on all machines that belong to the specified service.
// The id parameter can be either a service ID or name.
func (cli *Client) StartService(ctx context.Context, id string) error {
	snapshot, err := cli.InspectClusterSnapshot(ctx)
	if err != nil {
		return err
	}
	printSnapshotWarnings(snapshot.Warnings)

	svc, err := snapshot.Service(id)
	if err != nil {
		return err
	}

	wg := sync.WaitGroup{}
	errCh := make(chan error)

	// Start all containers on all machines that belong to the service.
	for _, mc := range svc.Containers {
		machine, err := snapshot.Machine(mc.MachineID)
		if err != nil {
			return fmt.Errorf("resolve machine '%s': %w", mc.MachineID, err)
		}
		wg.Go(func() {
			err := cli.StartContainerOnMachine(ctx, machine.Machine, mc.Container.ID, mc.Container.Name)
			if err != nil {
				errCh <- fmt.Errorf("start container '%s': %w", mc.Container.ID, err)
			}
		})
	}

	go func() {
		wg.Wait()
		close(errCh)
	}()

	err = nil
	for e := range errCh {
		err = errors.Join(err, e)
	}
	return err
}

// ListServices returns a list of all services and their containers.
func (cli *Client) ListServices(ctx context.Context) ([]api.Service, error) {
	snapshot, err := cli.InspectClusterSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	printSnapshotWarnings(snapshot.Warnings)
	return snapshot.Services, nil
}

func printSnapshotWarnings(warnings []string) {
	for _, warning := range warnings {
		fmt.Fprintf(os.Stderr, "WARNING: %s\n", warning)
	}
}
