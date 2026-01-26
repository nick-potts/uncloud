package client

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/psviderski/uncloud/internal/docker"
	machinedocker "github.com/psviderski/uncloud/internal/machine/docker"
	"github.com/psviderski/uncloud/internal/secret"
	"github.com/psviderski/uncloud/pkg/api"
	"google.golang.org/grpc/status"
)

// CreateContainer creates a new container for the given service on the specified machine.
func (cli *Client) CreateContainer(
	ctx context.Context, serviceID string, spec api.ServiceSpec, machineID string, opts api.CreateContainerOptions,
) (container.CreateResponse, error) {
	var resp container.CreateResponse

	spec = spec.SetDefaults()
	if err := spec.Validate(); err != nil {
		return resp, fmt.Errorf("invalid service spec: %w", err)
	}
	// TODO: validate spec.Name is consistent with serviceID if this is not the first container in the service.

	machine, err := cli.InspectMachine(ctx, machineID)
	if err != nil {
		return resp, fmt.Errorf("inspect machine '%s': %w", machineID, err)
	}

	suffix, err := secret.RandomAlphaNumeric(4)
	if err != nil {
		return resp, fmt.Errorf("generate random suffix: %w", err)
	}
	containerName := fmt.Sprintf("%s-%s", spec.Name, suffix)

	// Proxy Docker gRPC requests to the selected machine.
	ctx = proxyToMachine(ctx, machine.Machine)

	if spec.Container.PullPolicy == api.PullPolicyAlways {
		if err = cli.pullImage(ctx, spec.Container.Image, opts.OnPullProgress); err != nil {
			return resp, err
		}
	}

	resp, err = cli.Docker.CreateServiceContainer(ctx, serviceID, spec, containerName)
	if err != nil {
		switch spec.Container.PullPolicy {
		case api.PullPolicyAlways, api.PullPolicyNever:
			return resp, err
		case api.PullPolicyMissing:
		default:
			return resp, fmt.Errorf("unsupported pull policy: '%s'", spec.Container.PullPolicy)
		}

		// NotFound (No such image) error is expected if the image is missing.
		if !errdefs.IsNotFound(err) || !strings.Contains(err.Error(), "No such image") {
			return resp, err
		}

		// Pull the missing image and create the container again.
		if err = cli.pullImage(ctx, spec.Container.Image, opts.OnPullProgress); err != nil {
			return resp, err
		}
		if resp, err = cli.Docker.CreateServiceContainer(ctx, serviceID, spec, containerName); err != nil {
			return resp, err
		}
	}

	return resp, nil
}

// pullImage pulls an image from the registry, calling the optional callback with progress updates.
func (cli *Client) pullImage(ctx context.Context, image string, onProgress api.ImagePullCallback) error {
	opts := machinedocker.PullOptions{}
	// Try to retrieve the authentication token for the image from the default local Docker config file.
	if encodedAuth, err := docker.RetrieveLocalDockerRegistryAuth(image); err == nil {
		// If RegistryAuth is empty, Uncloud daemon will try to retrieve the credentials from its own Docker config.
		opts.RegistryAuth = encodedAuth
	}

	pullCh, err := cli.Docker.PullImage(ctx, image, opts)
	if err != nil {
		statusErr := status.Convert(err)
		pullErr := fmt.Errorf("pull image: %w", errors.New(statusErr.Message()))
		if onProgress != nil {
			onProgress(api.ImagePullProgress{Error: pullErr})
		}
		return pullErr
	}

	// Wait for pull to complete by reading all progress messages.
	var receivedDone bool
	for msg := range pullCh {
		if msg.Err != nil {
			err = msg.Err
		} else if msg.Message.Error != nil {
			err = errors.New(msg.Message.Error.Message)
		}
		if err != nil {
			statusErr := status.Convert(err)
			pullErr := fmt.Errorf("pull image: %w", errors.New(statusErr.Message()))
			if onProgress != nil {
				onProgress(api.ImagePullProgress{Error: pullErr})
			}
			return pullErr
		}

		if onProgress != nil {
			progress := toPullProgress(msg.Message)
			if progress != nil {
				onProgress(*progress)
				if progress.Done {
					receivedDone = true
				}
			}
		}
	}

	// Ensure we emit a final Done signal if the Docker API didn't send one.
	// This can happen with cached images or certain Docker versions.
	if onProgress != nil && !receivedDone {
		onProgress(api.ImagePullProgress{Done: true, Percent: 100, Status: "Pulled"})
	}

	return nil
}

// toPullProgress converts a JSON progress message from the Docker API to ImagePullProgress.
func toPullProgress(jm jsonmessage.JSONMessage) *api.ImagePullProgress {
	if jm.ID == "" {
		return nil
	}

	p := &api.ImagePullProgress{
		LayerID: jm.ID,
		Status:  jm.Status,
	}

	if jm.Progress != nil {
		p.Current = jm.Progress.Current
		p.Total = jm.Progress.Total
		if jm.Progress.Total > 0 {
			p.Percent = int(jm.Progress.Current * 100 / jm.Progress.Total)
		}
	}

	// Per-layer completion messages (don't set Done, these are just layer progress).
	switch jm.Status {
	case "Download complete", "Already exists", "Pull complete":
		p.Percent = 100
	}

	// Overall image pull completion messages (set Done to signal the entire pull is finished).
	if strings.Contains(jm.Status, "Image is up to date") ||
		strings.Contains(jm.Status, "Downloaded newer image") {
		p.Done = true
		p.Percent = 100
	}

	return p
}

// InspectContainer returns the information about the specified container within the service.
// containerNameOrID can be name, full ID, or ID prefix of the container.
func (cli *Client) InspectContainer(
	ctx context.Context, serviceNameOrID, containerNameOrID string,
) (api.MachineServiceContainer, error) {
	svc, err := cli.InspectService(ctx, serviceNameOrID)
	if err != nil {
		return api.MachineServiceContainer{}, fmt.Errorf("inspect service: %w", err)
	}

	prefixMatchCandidates := []api.MachineServiceContainer{}
	for _, c := range svc.Containers {
		if c.Container.ID == containerNameOrID ||
			c.Container.Name == containerNameOrID {
			return c, nil
		}

		if strings.HasPrefix(c.Container.ID, containerNameOrID) {
			prefixMatchCandidates = append(prefixMatchCandidates, c)
		}
	}

	if len(prefixMatchCandidates) == 1 {
		return prefixMatchCandidates[0], nil
	} else if len(prefixMatchCandidates) > 1 {
		return api.MachineServiceContainer{}, fmt.Errorf(
			"multiple containers found with ID prefix '%s'", containerNameOrID)
	}

	return api.MachineServiceContainer{}, api.ErrNotFound
}

// StartContainer starts the specified container within the service.
func (cli *Client) StartContainer(ctx context.Context, serviceNameOrID, containerNameOrID string) error {
	ctr, err := cli.InspectContainer(ctx, serviceNameOrID, containerNameOrID)
	if err != nil {
		return err
	}

	machine, err := cli.InspectMachine(ctx, ctr.MachineID)
	if err != nil {
		return fmt.Errorf("inspect machine '%s': %w", ctr.MachineID, err)
	}
	ctx = proxyToMachine(ctx, machine.Machine)

	return cli.Docker.StartContainer(ctx, ctr.Container.ID, container.StartOptions{})
}

// StopContainer stops the specified container within the service.
func (cli *Client) StopContainer(
	ctx context.Context, serviceNameOrID, containerNameOrID string, opts container.StopOptions,
) error {
	ctr, err := cli.InspectContainer(ctx, serviceNameOrID, containerNameOrID)
	if err != nil {
		return err
	}

	machine, err := cli.InspectMachine(ctx, ctr.MachineID)
	if err != nil {
		return fmt.Errorf("inspect machine '%s': %w", ctr.MachineID, err)
	}
	ctx = proxyToMachine(ctx, machine.Machine)

	return cli.Docker.StopContainer(ctx, ctr.Container.ID, opts)
}

// RemoveContainer removes the specified container within the service.
func (cli *Client) RemoveContainer(
	ctx context.Context, serviceNameOrID, containerNameOrID string, opts container.RemoveOptions,
) error {
	ctr, err := cli.InspectContainer(ctx, serviceNameOrID, containerNameOrID)
	if err != nil {
		return err
	}

	machine, err := cli.InspectMachine(ctx, ctr.MachineID)
	if err != nil {
		return fmt.Errorf("inspect machine '%s': %w", ctr.MachineID, err)
	}
	ctx = proxyToMachine(ctx, machine.Machine)

	return cli.Docker.RemoveServiceContainer(ctx, ctr.Container.ID, opts)
}

// ExecContainer executes a command in a container within the service.
// If containerNameOrID is empty, the first container in the service will be used.
func (cli *Client) ExecContainer(
	ctx context.Context, serviceNameOrID, containerNameOrID string, execOpts api.ExecOptions,
) (int, error) {
	var ctr api.MachineServiceContainer

	if containerNameOrID == "" {
		// Find the first (random) container in the service
		service, err := cli.InspectService(ctx, serviceNameOrID)
		if err != nil {
			return -1, fmt.Errorf("inspect service: %w", err)
		}
		if len(service.Containers) == 0 {
			return -1, fmt.Errorf("no containers found in service %s", serviceNameOrID)
		}
		ctr = service.Containers[0]
	} else {
		// Find the specific container
		var err error
		ctr, err = cli.InspectContainer(ctx, serviceNameOrID, containerNameOrID)
		if err != nil {
			return -1, fmt.Errorf("inspect container: %w", err)
		}
	}

	machine, err := cli.InspectMachine(ctx, ctr.MachineID)
	if err != nil {
		return -1, fmt.Errorf("inspect machine '%s': %w", ctr.MachineID, err)
	}

	// Proxy Docker gRPC requests to the machine hosting the container
	ctx = proxyToMachine(ctx, machine.Machine)

	// Execute the command in the container
	exitCode, err := cli.Docker.ExecContainer(ctx, machinedocker.ExecConfig{
		ContainerID: ctr.Container.ID,
		Options:     execOpts,
	})
	if err != nil {
		return exitCode, fmt.Errorf("exec in container %s: %w", ctr.Container.Name, err)
	}

	return exitCode, nil
}
