package client

import (
	"context"
	"net/netip"
	"testing"

	"github.com/psviderski/uncloud/internal/machine/api/pb"
	machinedocker "github.com/psviderski/uncloud/internal/machine/docker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestNewImagePruneFilters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		danglingOnly     bool
		expectedDangling string
	}{
		{
			name:             "all unused images by default",
			danglingOnly:     false,
			expectedDangling: "false",
		},
		{
			name:             "dangling only",
			danglingOnly:     true,
			expectedDangling: "true",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pruneFilters := newImagePruneFilters(tt.danglingOnly)
			assert.Equal(t, []string{tt.expectedDangling}, pruneFilters.Get("dangling"))
		})
	}
}

type mockImageDockerClient struct {
	pb.DockerClient
	pruneResp *pb.PruneImagesResponse
	pruneErr  error
}

func (m *mockImageDockerClient) PruneImages(ctx context.Context, in *pb.PruneImagesRequest, opts ...grpc.CallOption) (*pb.PruneImagesResponse, error) {
	return m.pruneResp, m.pruneErr
}

type mockImageClusterClient struct {
	pb.ClusterClient
	listMachinesResp *pb.ListMachinesResponse
	listMachinesErr  error
}

func (m *mockImageClusterClient) ListMachines(ctx context.Context, in *emptypb.Empty, opts ...grpc.CallOption) (*pb.ListMachinesResponse, error) {
	return m.listMachinesResp, m.listMachinesErr
}

func TestClientPruneImages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		opts             PruneImagesOptions
		machines         []*pb.MachineMember
		pruneResp        *pb.PruneImagesResponse
		expectedMachines []string
		expectedErr      string
		expectedPruneErr string
	}{
		{
			name: "single machine response with nil metadata is backfilled",
			opts: PruneImagesOptions{
				Machines: []string{"alpha"},
			},
			machines: []*pb.MachineMember{
				newTestMachineMember("machine-a", "alpha", "10.0.0.1"),
			},
			pruneResp: &pb.PruneImagesResponse{
				Messages: []*pb.MachineImagePruneReport{
					{
						Report: []byte(`{"ImagesDeleted":[{"Deleted":"sha256:deadbeef"}],"SpaceReclaimed":1234}`),
					},
				},
			},
			expectedMachines: []string{"machine-a"},
		},
		{
			name: "multi machine response rewrites management ip to machine id",
			opts: PruneImagesOptions{},
			machines: []*pb.MachineMember{
				newTestMachineMember("machine-a", "alpha", "10.0.0.1"),
				newTestMachineMember("machine-b", "beta", "10.0.0.2"),
			},
			pruneResp: &pb.PruneImagesResponse{
				Messages: []*pb.MachineImagePruneReport{
					{
						Metadata: &pb.Metadata{Machine: "10.0.0.1"},
						Report:   []byte(`{"SpaceReclaimed":10}`),
					},
					{
						Metadata: &pb.Metadata{Machine: "10.0.0.2"},
						Report:   []byte(`{"SpaceReclaimed":20}`),
					},
				},
			},
			expectedMachines: []string{"machine-a", "machine-b"},
		},
		{
			name: "partial failure returns reports and aggregated error with machine name",
			opts: PruneImagesOptions{},
			machines: []*pb.MachineMember{
				newTestMachineMember("machine-a", "alpha", "10.0.0.1"),
				newTestMachineMember("machine-b", "beta", "10.0.0.2"),
			},
			pruneResp: &pb.PruneImagesResponse{
				Messages: []*pb.MachineImagePruneReport{
					{
						Metadata: &pb.Metadata{Machine: "10.0.0.1"},
						Report:   []byte(`{"SpaceReclaimed":10}`),
					},
					{
						Metadata: &pb.Metadata{Machine: "10.0.0.2", Error: "boom"},
					},
				},
			},
			expectedMachines: []string{"machine-a", "machine-b"},
			expectedPruneErr: "prune images on machine 'beta': boom",
		},
		{
			name: "unknown machine filter returns error",
			opts: PruneImagesOptions{
				Machines: []string{"missing"},
			},
			machines: []*pb.MachineMember{
				newTestMachineMember("machine-a", "alpha", "10.0.0.1"),
			},
			expectedErr: "machines not found: missing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cli := &Client{
				ClusterClient: &mockImageClusterClient{
					listMachinesResp: &pb.ListMachinesResponse{Machines: tt.machines},
				},
				Docker: &machinedocker.Client{
					GRPCClient: &mockImageDockerClient{
						pruneResp: tt.pruneResp,
					},
				},
			}

			reports, err := cli.PruneImages(context.Background(), tt.opts)
			if tt.expectedErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectedErr)
				return
			}

			require.Len(t, reports, len(tt.expectedMachines))
			for i, machineID := range tt.expectedMachines {
				require.NotNil(t, reports[i].Metadata)
				assert.Equal(t, machineID, reports[i].Metadata.Machine)
			}

			if tt.expectedPruneErr == "" {
				require.NoError(t, err)
				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectedPruneErr)
		})
	}
}

func newTestMachineMember(id, name, managementIP string) *pb.MachineMember {
	return &pb.MachineMember{
		Machine: &pb.MachineInfo{
			Id:   id,
			Name: name,
			Network: &pb.NetworkConfig{
				ManagementIp: pb.NewIP(netip.MustParseAddr(managementIP)),
			},
		},
	}
}
