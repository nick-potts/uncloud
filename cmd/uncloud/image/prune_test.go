package image

import (
	"testing"

	"github.com/psviderski/uncloud/internal/machine/api/pb"
	"github.com/psviderski/uncloud/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolvePruneTargets(t *testing.T) {
	t.Parallel()

	allMachines := api.MachineMembersList{
		{Machine: &pb.MachineInfo{Id: "machine-a", Name: "alpha"}},
		{Machine: &pb.MachineInfo{Id: "machine-b", Name: "beta"}},
	}

	t.Run("all machines by default", func(t *testing.T) {
		t.Parallel()

		names, filters, err := resolvePruneTargets(allMachines, nil)
		require.NoError(t, err)
		assert.Equal(t, []string{"alpha", "beta"}, names)
		assert.Nil(t, filters)
	})

	t.Run("selected machines", func(t *testing.T) {
		t.Parallel()

		names, filters, err := resolvePruneTargets(allMachines, []string{"machine-b,alpha"})
		require.NoError(t, err)
		assert.Equal(t, []string{"alpha", "beta"}, names)
		assert.Equal(t, []string{"machine-b", "machine-a"}, filters)
	})

	t.Run("unknown machine", func(t *testing.T) {
		t.Parallel()

		_, _, err := resolvePruneTargets(allMachines, []string{"missing"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "machine 'missing' not found")
	})
}
