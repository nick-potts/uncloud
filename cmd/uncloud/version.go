package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/psviderski/uncloud/internal/cli"
	"github.com/psviderski/uncloud/internal/version"
	"github.com/psviderski/uncloud/pkg/client"
	"github.com/spf13/cobra"
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

	// List all machines in the cluster to get their states.
	machines, err := clusterClient.ListMachines(ctx, nil)
	if err != nil {
		fmt.Println("Cluster: (unavailable)")
		return nil
	}

	if len(machines) == 0 {
		fmt.Println("Cluster: (no machines)")
		return nil
	}

	// Broadcast InspectMachine to all available machines to get their versions.
	versions, err := inspectMachineVersions(ctx, clusterClient)
	if err != nil {
		return fmt.Errorf("inspect machine versions: %w", err)
	}

	// Print machine versions in a table format.
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	if _, err = fmt.Fprintln(tw, "MACHINE\tSTATE\tVERSION"); err != nil {
		return fmt.Errorf("write header: %w", err)
	}

	for _, m := range machines {
		machineName := m.Machine.Name
		state := capitalise(m.State.String())

		ver := "(unreachable)"
		if v, ok := versions[machineName]; ok {
			ver = v
		}

		if _, err = fmt.Fprintf(tw, "%s\t%s\t%s\n", machineName, state, ver); err != nil {
			return fmt.Errorf("write row: %w", err)
		}
	}

	return tw.Flush()
}

// inspectMachineVersions broadcasts InspectMachine to all available machines and returns a map of machine name to version.
func inspectMachineVersions(ctx context.Context, c *client.Client) (map[string]string, error) {
	// Create a context that proxies to all available (non-DOWN) machines.
	proxyCtx, availableMachines, err := c.ProxyMachinesContext(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("proxy machines context: %w", err)
	}

	// Build a map of management IP to machine name for resolving response metadata.
	machineNamesByIP := make(map[string]string)
	for _, m := range availableMachines {
		if addr, err := m.Machine.Network.ManagementIp.ToAddr(); err == nil {
			machineNamesByIP[addr.String()] = m.Machine.Name
		}
	}

	// Broadcast InspectMachine to all machines.
	resp, err := c.MachineClient.InspectMachine(proxyCtx, nil)
	if err != nil {
		return nil, fmt.Errorf("inspect machines: %w", err)
	}

	versions := make(map[string]string)
	for _, details := range resp.Machines {
		var machineName string
		if details.Metadata != nil {
			machineName = machineNamesByIP[details.Metadata.Machine]
			if details.Metadata.Error != "" {
				client.PrintWarning(fmt.Sprintf("failed to get version from machine %s: %s",
					machineName, details.Metadata.Error))
				continue
			}
		} else if len(resp.Machines) == 1 && len(availableMachines) == 1 {
			// Single machine response without metadata.
			machineName = availableMachines[0].Machine.Name
		}

		if machineName != "" {
			versions[machineName] = versionOrDev(details.DaemonVersion)
		}
	}

	return versions, nil
}

// versionOrDev returns "(dev)" if the version is empty, otherwise returns the version as-is.
func versionOrDev(v string) string {
	if v == "" {
		return "(dev)"
	}
	return v
}

// capitalise returns a string where the first character is upper case, and the rest is lower case.
func capitalise(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}
