package client

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/psviderski/uncloud/internal/machine/api/pb"
	"github.com/psviderski/uncloud/pkg/api"
	"google.golang.org/grpc/metadata"
)

// ClusterSnapshot is a point-in-time view of cluster machines and service containers.
// It is intentionally explicit and short-lived, built per command or planning pass.
type ClusterSnapshot struct {
	Machines api.MachineMembersList
	Services []api.Service
	Warnings []string

	machineByAddr     map[string]*pb.MachineMember
	machineByID       map[string]*pb.MachineMember
	serviceByID       map[string]api.Service
	serviceIDsByName  map[string][]string
	availableMachines api.MachineMembersList
}

// InspectClusterSnapshot returns a live snapshot built from the current machine list and broadcast container list.
func (cli *Client) InspectClusterSnapshot(ctx context.Context) (*ClusterSnapshot, error) {
	machines, err := cli.ListMachines(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("list machines: %w", err)
	}

	snapshot := &ClusterSnapshot{
		Machines:         machines,
		machineByAddr:    make(map[string]*pb.MachineMember, len(machines)),
		machineByID:      make(map[string]*pb.MachineMember, len(machines)),
		serviceByID:      make(map[string]api.Service),
		serviceIDsByName: make(map[string][]string),
	}

	md := metadata.New(nil)
	for _, machine := range machines {
		snapshot.machineByID[machine.Machine.Id] = machine
		addr, err := machine.Machine.Network.ManagementIp.ToAddr()
		if err == nil {
			snapshot.machineByAddr[addr.String()] = machine
		}

		if machine.State == pb.MachineMember_UP || machine.State == pb.MachineMember_SUSPECT {
			if err == nil {
				md.Append("machines", addr.String())
				snapshot.availableMachines = append(snapshot.availableMachines, machine)
			}
		}
	}

	listCtx := metadata.NewOutgoingContext(ctx, md)
	machineContainers, err := cli.Docker.ListServiceContainers(listCtx, "", container.ListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}

	for _, mc := range machineContainers {
		if mc.Metadata == nil && len(machineContainers) > 1 {
			return nil, errors.New("something went wrong with gRPC proxy: metadata is missing for a machine response")
		}
		if mc.Metadata != nil && mc.Metadata.Error != "" {
			machine := mc.Metadata.Machine
			if m := snapshot.machineByAddr[mc.Metadata.Machine]; m != nil {
				machine = m.Machine.Name
			}
			snapshot.Warnings = append(snapshot.Warnings,
				fmt.Sprintf("failed to list containers on machine '%s': %s", machine, mc.Metadata.Error))
			continue
		}

		var machine *pb.MachineMember
		if mc.Metadata == nil {
			if len(snapshot.availableMachines) != 1 {
				return nil, errors.New("single-machine response is ambiguous without proxy metadata")
			}
			machine = snapshot.availableMachines[0]
		} else {
			machine = snapshot.machineByAddr[mc.Metadata.Machine]
			if machine == nil {
				return nil, fmt.Errorf("machine not found by management IP: %s", mc.Metadata.Machine)
			}
		}

		for _, ctr := range mc.Containers {
			svc := snapshot.serviceByID[ctr.ServiceID()]
			if svc.ID == "" {
				svc = api.Service{
					ID:   ctr.ServiceID(),
					Name: ctr.ServiceName(),
					Mode: ctr.ServiceMode(),
				}
				if svc.Mode == "" {
					svc.Mode = api.ServiceModeReplicated
				}
			}
			svc.Containers = append(svc.Containers, api.MachineServiceContainer{
				MachineID: machine.Machine.Id,
				Container: ctr,
			})
			snapshot.serviceByID[svc.ID] = svc
		}
	}

	snapshot.Services = make([]api.Service, 0, len(snapshot.serviceByID))
	for _, svc := range snapshot.serviceByID {
		snapshot.Services = append(snapshot.Services, svc)
		snapshot.serviceIDsByName[svc.Name] = append(snapshot.serviceIDsByName[svc.Name], svc.ID)
	}
	slices.SortFunc(snapshot.Services, func(a, b api.Service) int {
		if a.Name == b.Name {
			return strings.Compare(a.ID, b.ID)
		}
		return strings.Compare(a.Name, b.Name)
	})

	return snapshot, nil
}

func (s *ClusterSnapshot) Machine(nameOrID string) (*pb.MachineMember, error) {
	if machine := s.Machines.FindByNameOrID(nameOrID); machine != nil {
		return machine, nil
	}
	return nil, api.ErrNotFound
}

func (s *ClusterSnapshot) MachineName(machineID string) string {
	if machine, ok := s.machineByID[machineID]; ok {
		return machine.Machine.Name
	}
	return machineID
}

func (s *ClusterSnapshot) Service(nameOrID string) (api.Service, error) {
	if svc, ok := s.serviceByID[nameOrID]; ok {
		return svc, nil
	}

	serviceIDs := s.serviceIDsByName[nameOrID]
	switch len(serviceIDs) {
	case 0:
		return api.Service{}, api.ErrNotFound
	case 1:
		return s.serviceByID[serviceIDs[0]], nil
	default:
		return api.Service{}, fmt.Errorf("multiple services found with name '%s', use the service ID instead", nameOrID)
	}
}

func (s *ClusterSnapshot) FilterMachines(namesOrIDs []string) (api.MachineMembersList, error) {
	if len(namesOrIDs) == 0 {
		return s.Machines, nil
	}

	filtered := make(api.MachineMembersList, 0, len(namesOrIDs))
	var notFound []string
	for _, nameOrID := range namesOrIDs {
		machine, err := s.Machine(nameOrID)
		if err != nil {
			notFound = append(notFound, nameOrID)
			continue
		}
		filtered = append(filtered, machine)
	}
	if len(notFound) > 0 {
		return nil, fmt.Errorf("machines not found: %s", strings.Join(notFound, ", "))
	}
	return filtered, nil
}
