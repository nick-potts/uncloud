package client

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/docker/docker/pkg/stringid"
	"github.com/psviderski/uncloud/internal/machine/api/pb"
	"github.com/psviderski/uncloud/pkg/api"
)

// ServiceLogs streams log entries from all service containers in chronological order based on timestamps.
// Keep in mind that perfect ordering of log events across multiple machines can't be guaranteed due to the
// imperfection of physical clocks or potential clock skew between machines.
// It uses a low watermark algorithm to ensure proper ordering across multiple machines.
// Heartbeat entries from the server advance the watermark to enable timely emission of buffered logs.
func (cli *Client) ServiceLogs(
	ctx context.Context, serviceNameOrID string, opts api.ServiceLogsOptions,
) (api.Service, <-chan api.ServiceLogEntry, error) {
	snapshot, err := cli.InspectClusterSnapshot(ctx)
	if err != nil {
		return api.Service{}, nil, fmt.Errorf("inspect cluster snapshot: %w", err)
	}
	printSnapshotWarnings(snapshot.Warnings)

	return cli.ServiceLogsFromSnapshot(ctx, snapshot, serviceNameOrID, opts)
}

func (cli *Client) ServiceLogsFromSnapshot(
	ctx context.Context, snapshot *ClusterSnapshot, serviceNameOrID string, opts api.ServiceLogsOptions,
) (api.Service, <-chan api.ServiceLogEntry, error) {
	svc, err := snapshot.Service(serviceNameOrID)
	if err != nil {
		return svc, nil, fmt.Errorf("inspect service: %w", err)
	}

	if len(svc.Containers) == 0 {
		return svc, nil, fmt.Errorf("no containers found for service: %s", serviceNameOrID)
	}

	machines, err := snapshot.FilterMachines(opts.Machines)
	if err != nil {
		return svc, nil, fmt.Errorf("list machines: %w", err)
	}

	ctrStreams := make([]<-chan api.ServiceLogEntry, 0, len(svc.Containers))
	for _, ctr := range svc.Containers {
		// Skip containers not running on the specified machines.
		m := machines.FindByNameOrID(ctr.MachineID)
		if len(opts.Machines) > 0 && m == nil {
			continue
		}
		if m == nil {
			m, err = snapshot.Machine(ctr.MachineID)
			if err != nil {
				return svc, nil, fmt.Errorf("resolve machine '%s': %w", ctr.MachineID, err)
			}
		}

		// Machine name for ServiceLogEntry metadata and friendlier error message.
		machineName := m.Machine.Name

		stream, err := cli.ContainerLogsOnMachine(ctx, m.Machine, ctr.Container.ID, opts)
		if err != nil {
			return svc, nil, fmt.Errorf("stream logs from service container '%s' on machine '%s': %w",
				stringid.TruncateID(ctr.Container.ID), machineName, err)
		}

		// Enrich log entries from the container with service metadata.
		metadata := api.ServiceLogEntryMetadata{
			ServiceID:   svc.ID,
			ServiceName: svc.Name,
			ContainerID: ctr.Container.ID,
			MachineID:   ctr.MachineID,
			MachineName: machineName,
		}
		enrichedStream := logsStreamWithServiceMetadata(stream, metadata)
		ctrStreams = append(ctrStreams, enrichedStream)
	}

	if len(ctrStreams) == 0 {
		return svc, nil, errors.New("no service containers found on the specified machine(s)")
	}

	// Use the log merger to combine streams from all containers in chronological order.
	merger := NewLogMerger(ctrStreams, DefaultLogMergerOptions)
	mergedStream := merger.Stream()

	return svc, mergedStream, nil
}

// ContainerLogs streams log entries from a single container on a specified machine.
func (cli *Client) ContainerLogs(
	ctx context.Context, machineNameOrID string, containerID string, opts api.ServiceLogsOptions,
) (<-chan api.ContainerLogEntry, error) {
	machine, err := cli.InspectMachine(ctx, machineNameOrID)
	if err != nil {
		return nil, fmt.Errorf("inspect machine '%s': %w", machineNameOrID, err)
	}

	return cli.ContainerLogsOnMachine(ctx, machine.Machine, containerID, opts)
}

func (cli *Client) ContainerLogsOnMachine(
	ctx context.Context, machine *pb.MachineInfo, containerID string, opts api.ServiceLogsOptions,
) (<-chan api.ContainerLogEntry, error) {
	proxyCtx := proxyToMachine(ctx, machine)
	stream, err := cli.Docker.GRPCClient.ContainerLogs(proxyCtx, containerLogsRequest(containerID, opts))
	if err != nil {
		return nil, err
	}

	return containerLogsStream(ctx, stream), nil
}

func containerLogsRequest(containerID string, opts api.ServiceLogsOptions) *pb.ContainerLogsRequest {
	req := &pb.ContainerLogsRequest{
		ContainerId: containerID,
		Follow:      opts.Follow,
		Tail:        int32(opts.Tail),
		Since:       opts.Since,
		Until:       opts.Until,
	}
	if !opts.Follow && opts.Tail == 0 {
		// If not following and tail is 0, set tail to -1 to return all logs.
		// Otherwise, no logs will be returned at all.
		req.Tail = -1
	}

	return req
}

func containerLogsStream(
	ctx context.Context, stream pb.Docker_ContainerLogsClient,
) <-chan api.ContainerLogEntry {
	ch := make(chan api.ContainerLogEntry)

	go func() {
		defer close(ch)

		for {
			pbEntry, err := stream.Recv()
			if err == io.EOF {
				return
			}
			if err != nil {
				ch <- api.ContainerLogEntry{
					Err: err,
				}
				return
			}

			entry := api.ContainerLogEntry{
				Stream:    api.LogStreamTypeFromProto(pbEntry.Stream),
				Message:   pbEntry.Message,
				Timestamp: pbEntry.Timestamp.AsTime(),
			}

			select {
			case ch <- entry:
			case <-ctx.Done():
				return
			}
		}
	}()

	return ch
}

// logsStreamWithServiceMetadata wraps a container logs stream and enriches each log entry with service metadata.
func logsStreamWithServiceMetadata(
	stream <-chan api.ContainerLogEntry, metadata api.ServiceLogEntryMetadata,
) <-chan api.ServiceLogEntry {
	out := make(chan api.ServiceLogEntry)

	go func() {
		for entry := range stream {
			out <- api.ServiceLogEntry{
				Metadata:          metadata,
				ContainerLogEntry: entry,
			}
		}
		close(out)
	}()

	return out
}
