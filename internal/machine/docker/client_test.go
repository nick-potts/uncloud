package docker

import (
	"testing"

	"github.com/docker/docker/api/types/image"
	"github.com/psviderski/uncloud/internal/machine/api/pb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseMachineImagePruneReports(t *testing.T) {
	t.Parallel()

	reportJSON := []byte(`{"ImagesDeleted":[{"Deleted":"sha256:deadbeef"},{"Untagged":"example:latest"}],"SpaceReclaimed":1234}`)
	reports, err := parseMachineImagePruneReports([]*pb.MachineImagePruneReport{
		{
			Metadata: &pb.Metadata{Machine: "machine-1"},
			Report:   reportJSON,
		},
		{
			Metadata: &pb.Metadata{Machine: "machine-2", Error: "boom"},
		},
	})
	require.NoError(t, err)
	require.Len(t, reports, 2)

	assert.Equal(t, "machine-1", reports[0].Metadata.Machine)
	assert.Equal(t, image.PruneReport{
		ImagesDeleted: []image.DeleteResponse{
			{Deleted: "sha256:deadbeef"},
			{Untagged: "example:latest"},
		},
		SpaceReclaimed: 1234,
	}, reports[0].Report)

	assert.Equal(t, "machine-2", reports[1].Metadata.Machine)
	assert.Equal(t, "boom", reports[1].Metadata.Error)
	assert.Empty(t, reports[1].Report.ImagesDeleted)
	assert.Zero(t, reports[1].Report.SpaceReclaimed)
}

func TestParseMachineImagePruneReports_InvalidJSON(t *testing.T) {
	t.Parallel()

	_, err := parseMachineImagePruneReports([]*pb.MachineImagePruneReport{
		{
			Metadata: &pb.Metadata{Machine: "machine-1"},
			Report:   []byte(`{"ImagesDeleted":`),
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unmarshal image prune report")
}
