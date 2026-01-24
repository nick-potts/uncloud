package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/psviderski/uncloud/internal/cli"
	"github.com/psviderski/uncloud/internal/machine/api/pb"
	"github.com/psviderski/uncloud/internal/version"
	"github.com/psviderski/uncloud/pkg/client"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func NewVersionCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Show client and server version information.",
		Long: `Show version information for both the local client and all machines in the cluster.

The client version is always shown. If connected to a cluster, the version of the
daemon running on each machine is also displayed.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			uncli := cmd.Context().Value("cli").(*cli.CLI)
			return runVersion(cmd.Context(), uncli)
		},
	}
	return cmd
}

func runVersion(ctx context.Context, uncli *cli.CLI) error {
	fmt.Printf("Client: %s\n", versionOrDev(version.String()))
	fmt.Println()

	// Try to connect to the cluster to get server versions.
	clusterClient, err := uncli.ConnectClusterWithOptions(ctx, cli.ConnectOptions{
		ShowProgress: false,
	})
	if err != nil {
		fmt.Println("Cluster: (not connected)")
		return nil
	}
	defer clusterClient.Close()

	// List all machines in the cluster.
	machines, err := clusterClient.ListMachines(ctx, nil)
	if err != nil {
		fmt.Println("Cluster: (unavailable)")
		return nil
	}

	if len(machines) == 0 {
		fmt.Println("Cluster: (no machines)")
		return nil
	}

	// Print machine versions in a table format.
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	if _, err = fmt.Fprintln(tw, "MACHINE\tSTATE\tVERSION"); err != nil {
		return fmt.Errorf("write header: %w", err)
	}

	for _, m := range machines {
		machineName := m.Machine.Name
		state := capitaliseState(m.State)
		ver := getMachineVersion(ctx, clusterClient, m)

		if _, err = fmt.Fprintf(tw, "%s\t%s\t%s\n", machineName, state, ver); err != nil {
			return fmt.Errorf("write row: %w", err)
		}
	}

	return tw.Flush()
}

// getMachineVersion retrieves the version from a specific machine, handling various error cases.
func getMachineVersion(ctx context.Context, c *client.Client, m *pb.MachineMember) string {
	// Skip machines that are DOWN since we can't connect to them.
	if m.State == pb.MachineMember_DOWN {
		return "(unreachable)"
	}

	// Proxy the request to the specific machine.
	machineCtx := c.ProxyToMachine(ctx, m)

	ver, err := c.GetVersion(machineCtx)
	if err != nil {
		// Check if the machine is running an older version that doesn't have GetVersion.
		if status.Code(err) == codes.Unimplemented {
			return "(unknown - daemon version < 0.17)"
		}
		// Handle connection errors or other issues.
		if status.Code(err) == codes.Unavailable {
			return "(unreachable)"
		}
		return fmt.Sprintf("(error: %v)", err)
	}

	return versionOrDev(ver)
}

// versionOrDev returns "(dev)" if the version is empty, otherwise returns the version as-is.
func versionOrDev(v string) string {
	if v == "" {
		return "(dev)"
	}
	return v
}

// capitaliseState returns a human-readable state string.
func capitaliseState(state pb.MachineMember_MembershipState) string {
	switch state {
	case pb.MachineMember_UP:
		return "Up"
	case pb.MachineMember_SUSPECT:
		return "Suspect"
	case pb.MachineMember_DOWN:
		return "Down"
	default:
		return "Unknown"
	}
}
