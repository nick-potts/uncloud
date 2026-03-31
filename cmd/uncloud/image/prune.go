package image

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/docker/compose/v2/pkg/progress"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/go-units"
	"github.com/psviderski/uncloud/internal/cli"
	"github.com/psviderski/uncloud/internal/cli/tui"
	"github.com/psviderski/uncloud/pkg/api"
	"github.com/psviderski/uncloud/pkg/client"
	"github.com/spf13/cobra"
)

type pruneOptions struct {
	danglingOnly bool
	machines     []string
	yes          bool
}

func NewPruneCommand() *cobra.Command {
	opts := pruneOptions{}

	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove unused images on machines in the cluster.",
		Long: `Remove unused images on machines in the cluster.

By default, all unused images are removed. Use --dangling-only to remove only
dangling images.`,
		Example: `  # Prune all unused images on all machines.
  uc image prune

  # Prune only dangling images on all machines.
  uc image prune --dangling-only

  # Prune only dangling images on specific machines without confirmation.
  uc image prune --dangling-only -m machine1,machine2 --yes`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			uncli := cmd.Context().Value("cli").(*cli.CLI)
			return prune(cmd.Context(), uncli, opts)
		},
	}

	cmd.Flags().BoolVarP(&opts.danglingOnly, "dangling-only", "d", false,
		"Remove only dangling images.")
	cmd.Flags().StringSliceVarP(&opts.machines, "machine", "m", nil,
		"Machine names or IDs to prune images on. Can be specified multiple times or as a comma-separated list.\n"+
			"If not specified, images are pruned on all machines.")
	cmd.Flags().BoolVarP(&opts.yes, "yes", "y", false,
		"Do not prompt for confirmation before pruning images.")

	return cmd
}

func prune(ctx context.Context, uncli *cli.CLI, opts pruneOptions) error {
	clusterClient, err := uncli.ConnectCluster(ctx)
	if err != nil {
		return fmt.Errorf("connect to cluster: %w", err)
	}
	defer clusterClient.Close()

	var allMachines api.MachineMembersList
	var targetMachineNames []string
	var machineFilters []string
	var pruneCandidates []machinePruneCandidates
	err = progress.RunWithTitle(ctx, func(ctx context.Context) error {
		pw := progress.ContextWriter(ctx)
		eventID := "Cluster images"
		pw.Event(progress.NewEvent(eventID, progress.Working, "Inspecting"))
		defer pw.Event(progress.NewEvent(eventID, progress.Done, "Inspected"))

		var err error
		allMachines, err = clusterClient.ListMachines(ctx, nil)
		if err != nil {
			return fmt.Errorf("list machines: %w", err)
		}
		if len(allMachines) == 0 {
			return fmt.Errorf("no machines found")
		}

		targetMachineNames, machineFilters, err = resolvePruneTargets(allMachines, opts.machines)
		if err != nil {
			return err
		}

		pruneCandidates, err = collectPruneCandidates(ctx, clusterClient, allMachines, machineFilters, opts.danglingOnly)
		if err != nil {
			return fmt.Errorf("collect prune candidates: %w", err)
		}
		return nil
	}, uncli.ProgressOut(), "Inspecting images on cluster")
	if err != nil {
		return err
	}
	if !hasPruneCandidates(pruneCandidates) {
		scope := "unused images"
		if opts.danglingOnly {
			scope = "dangling images"
		}
		fmt.Printf("No %s found on: %s\n", scope, strings.Join(targetMachineNames, ", "))
		return nil
	}

	fmt.Println(tui.Bold.Underline(true).Render("Image prune plan"))
	fmt.Println()

	directConn := uncli.DirectConnection()
	contextName := uncli.ContextOverrideOrCurrent()
	pruneTarget := ""
	if directConn != "" {
		pruneTarget = directConn
		fmt.Println(tui.Faint.Render("connection: ") + tui.NameStyle.Render(directConn))
		fmt.Println()
	} else if contextName != "" && len(uncli.Config.Contexts) > 1 {
		pruneTarget = contextName
		fmt.Println(tui.Faint.Render("context: ") + tui.NameStyle.Render(contextName))
		fmt.Println()
	}

	fmt.Println(formatPrunePlan(opts.danglingOnly, pruneCandidates))
	fmt.Println()

	if !opts.yes {
		title := "Proceed with image prune?"
		if pruneTarget != "" {
			isDark := lipgloss.HasDarkBackground(os.Stdin, os.Stdout)
			confirmStyle := tui.ThemeConfirm().Theme(isDark).Focused.Title
			title = "Proceed with image prune on " + tui.NameStyle.Render(pruneTarget) + confirmStyle.Render("?")
		}

		confirmed, err := tui.Confirm(title)
		if err != nil {
			return fmt.Errorf("confirm prune: %w", err)
		}
		if !confirmed {
			fmt.Println("Cancelled. No images were pruned.")
			return nil
		}
	}

	reports, pruneErr := clusterClient.PruneImages(ctx, client.PruneImagesOptions{
		DanglingOnly: opts.danglingOnly,
		Machines:     machineFilters,
	})
	if pruneErr != nil && len(reports) == 0 {
		return pruneErr
	}

	var totalReclaimed uint64
	for _, report := range reports {
		machineName := "<unknown>"
		if report.Metadata != nil {
			machineName = report.Metadata.Machine
			if m := allMachines.FindByNameOrID(machineName); m != nil {
				machineName = m.Machine.Name
			}
		}

		if report.Metadata != nil && report.Metadata.Error != "" {
			tui.PrintWarning(fmt.Sprintf("failed to prune images on machine '%s': %s",
				machineName, report.Metadata.Error))
			continue
		}

		printPruneReport(machineName, report.Report)
		totalReclaimed += report.Report.SpaceReclaimed
	}

	fmt.Printf("Total reclaimed space: %s\n", units.HumanSize(float64(totalReclaimed)))

	return pruneErr
}

func resolvePruneTargets(
	allMachines api.MachineMembersList, machineFilters []string,
) (machineNames []string, resolvedFilters []string, err error) {
	filters := cli.ExpandCommaSeparatedValues(machineFilters)
	if len(filters) == 0 {
		machineNames = make([]string, 0, len(allMachines))
		for _, machine := range allMachines {
			machineNames = append(machineNames, machine.Machine.Name)
		}
		sort.Strings(machineNames)
		return machineNames, nil, nil
	}

	seen := make(map[string]struct{}, len(filters))
	for _, filter := range filters {
		machine := allMachines.FindByNameOrID(filter)
		if machine == nil {
			return nil, nil, fmt.Errorf("machine '%s' not found", filter)
		}
		if _, ok := seen[machine.Machine.Id]; ok {
			continue
		}
		seen[machine.Machine.Id] = struct{}{}
		machineNames = append(machineNames, machine.Machine.Name)
		resolvedFilters = append(resolvedFilters, machine.Machine.Id)
	}

	sort.Strings(machineNames)
	return machineNames, resolvedFilters, nil
}

type machinePruneCandidates struct {
	machineName string
	images      []string
}

func collectPruneCandidates(
	ctx context.Context, clusterClient *client.Client, allMachines api.MachineMembersList, machineFilters []string, danglingOnly bool,
) ([]machinePruneCandidates, error) {
	machineImages, err := clusterClient.ListImages(ctx, api.ImageFilter{Machines: machineFilters})
	if err != nil {
		return nil, err
	}

	candidates := make([]machinePruneCandidates, 0, len(machineImages))
	var collectErr error
	for _, mi := range machineImages {
		machineName := "<unknown>"
		if mi.Metadata != nil {
			machineName = mi.Metadata.Machine
			if m := allMachines.FindByNameOrID(machineName); m != nil {
				machineName = m.Machine.Name
			}
		}

		if mi.Metadata != nil && mi.Metadata.Error != "" {
			collectErr = errors.Join(collectErr, fmt.Errorf("list images on machine '%s': %s",
				machineName, mi.Metadata.Error))
			continue
		}

		images := make([]string, 0, len(mi.Images))
		for _, img := range mi.Images {
			if !matchesPruneCandidate(img, danglingOnly) {
				continue
			}
			images = append(images, formatPruneCandidate(img))
		}
		sort.Strings(images)
		candidates = append(candidates, machinePruneCandidates{
			machineName: machineName,
			images:      images,
		})
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].machineName < candidates[j].machineName
	})

	if collectErr != nil {
		return nil, collectErr
	}

	return candidates, nil
}

func matchesPruneCandidate(img image.Summary, danglingOnly bool) bool {
	if img.Containers != 0 {
		return false
	}

	if danglingOnly {
		return isDanglingImage(img)
	}

	return true
}

func isDanglingImage(img image.Summary) bool {
	if len(img.RepoTags) == 0 {
		return true
	}

	for _, tag := range img.RepoTags {
		if tag != "<none>:<none>" {
			return false
		}
	}

	return true
}

func formatPruneCandidate(img image.Summary) string {
	for _, tag := range img.RepoTags {
		if tag != "<none>:<none>" {
			return tag
		}
	}

	id := strings.TrimPrefix(img.ID, "sha256:")
	if len(id) > 12 {
		id = id[:12]
	}

	return fmt.Sprintf("<none> (%s)", id)
}

func hasPruneCandidates(candidates []machinePruneCandidates) bool {
	for _, machine := range candidates {
		if len(machine.images) > 0 {
			return true
		}
	}

	return false
}

func formatPrunePlan(danglingOnly bool, candidates []machinePruneCandidates) string {
	scope := "all unused images"
	if danglingOnly {
		scope = "dangling images"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s\n", tui.Faint.Render("scope"), scope)

	machineCount := 0
	imageCount := 0
	for _, machine := range candidates {
		if len(machine.images) == 0 {
			continue
		}
		machineCount++
		imageCount += len(machine.images)
	}

	summary := fmt.Sprintf("%d image", imageCount)
	if imageCount != 1 {
		summary += "s"
	}
	summary += fmt.Sprintf(" on %d machine", machineCount)
	if machineCount != 1 {
		summary += "s"
	}
	fmt.Fprintf(&b, "%s: %s\n", tui.Faint.Render("summary"), summary)
	fmt.Fprintf(&b, "%s\n", tui.Faint.Render(strings.Repeat("─", lipgloss.Width(summary)+9)))

	for i, machine := range candidates {
		if len(machine.images) == 0 {
			continue
		}
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(tui.NameStyle.Render(machine.machineName))
		b.WriteString("\n")
		for _, imageName := range machine.images {
			fmt.Fprintf(&b, "  • %s\n", formatPrunePlanImage(imageName))
		}
	}

	return strings.TrimRight(b.String(), "\n")
}

func formatPrunePlanImage(imageName string) string {
	if strings.HasPrefix(imageName, "<none> (") {
		return tui.Faint.Render(imageName)
	}
	return tui.FormatImage(imageName, tui.NoStyle)
}

func printPruneReport(machineName string, report image.PruneReport) {
	fmt.Printf("Machine '%s':\n", machineName)
	if len(report.ImagesDeleted) == 0 {
		fmt.Println("No images deleted.")
	} else {
		fmt.Println("Deleted images:")
		for _, deleted := range report.ImagesDeleted {
			switch {
			case deleted.Untagged != "":
				fmt.Printf("untagged: %s\n", deleted.Untagged)
			case deleted.Deleted != "":
				fmt.Printf("deleted: %s\n", deleted.Deleted)
			}
		}
	}
	fmt.Printf("Reclaimed space: %s\n\n", units.HumanSize(float64(report.SpaceReclaimed)))
}
