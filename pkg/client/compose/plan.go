package compose

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/psviderski/uncloud/internal/cli/tui"
	"github.com/psviderski/uncloud/pkg/api"
	"github.com/psviderski/uncloud/pkg/client/deploy"
	"github.com/psviderski/uncloud/pkg/client/deploy/operation"
)

// Plan holds the compose-level deployment plan with volume and service operations.
type Plan struct {
	Volumes  []*operation.CreateVolumeOperation
	Services []*deploy.ServicePlan

	serviceDependencies map[string][]string
}

// IsEmpty returns true if the plan has no volume or service operations.
func (p *Plan) IsEmpty() bool {
	return len(p.Volumes) == 0 && len(p.Services) == 0
}

// Format renders the entire deployment plan as a styled tree with a summary footer.
func (p *Plan) Format() string {
	var out strings.Builder

	// Format volume operations.
	for _, op := range p.Volumes {
		out.WriteString(op.Format())
		out.WriteString("\n")
	}
	if len(p.Volumes) > 0 {
		out.WriteString("\n")
	}

	// Format service plans.
	for _, svcPlan := range p.Services {
		out.WriteString(svcPlan.Format())
		out.WriteString("\n")
	}

	// Format summary footer.
	summary := p.formatSummary()
	out.WriteString(tui.Faint.Render(strings.Repeat("─", lipgloss.Width(summary))))
	out.WriteString("\n")
	out.WriteString(summary)
	out.WriteString("\n")

	return out.String()
}

// formatSummary counts all operations across the plan and renders the summary footer.
func (p *Plan) formatSummary() string {
	var createCount, startFirstCount, stopFirstCount, removeCount int
	machines := make(map[string]struct{})

	for _, op := range p.Volumes {
		machines[op.MachineID] = struct{}{}
		createCount++
	}

	for _, svcPlan := range p.Services {
		for _, op := range svcPlan.Operations {
			switch o := op.(type) {
			case *operation.RunContainerOperation:
				machines[o.MachineID] = struct{}{}
				createCount++
			case *operation.ReplaceContainerOperation:
				machines[o.MachineID] = struct{}{}
				if o.Order == api.UpdateOrderStopFirst {
					stopFirstCount++
				} else {
					startFirstCount++
				}
			case *operation.RemoveContainerOperation:
				machines[o.MachineID] = struct{}{}
				removeCount++
			case *operation.StopContainerOperation:
				machines[o.MachineID] = struct{}{}
				removeCount++
			}
		}
	}

	var parts []string
	if createCount > 0 {
		parts = append(parts,
			tui.BoldGreen.Render(strconv.Itoa(createCount))+" "+tui.Green.Render("create"))
	}
	if startFirstCount > 0 {
		parts = append(parts,
			tui.BoldGreen.Render(strconv.Itoa(startFirstCount))+" "+tui.Green.Render("replace (start-first)"))
	}
	if stopFirstCount > 0 {
		parts = append(parts,
			tui.BoldYellow.Render(strconv.Itoa(stopFirstCount))+" "+tui.Yellow.Render("replace (stop-first)"))
	}
	if removeCount > 0 {
		parts = append(parts,
			tui.BoldRed.Render(strconv.Itoa(removeCount))+" "+tui.Red.Render("remove"))
	}

	machinesWord := "machines"
	if len(machines) == 1 {
		machinesWord = "machine"
	}
	parts = append(parts, fmt.Sprintf("across %s %s", tui.Bold.Render(strconv.Itoa(len(machines))), machinesWord))

	sep := " " + tui.Faint.Render("·") + " "
	return strings.Join(parts, sep)
}

// Execute runs all volume operations followed by all service operations.
func (p *Plan) Execute(ctx context.Context, cli operation.Client) error {
	for _, op := range p.Volumes {
		if err := op.Execute(ctx, cli); err != nil {
			return err
		}
	}
	for _, sp := range p.Services {
		if err := sp.Execute(ctx, cli); err != nil {
			return err
		}
	}
	return nil
}

// ExecuteParallel runs volume operations first, then deploys independent services in parallel
// while preserving dependency order between service batches.
func (p *Plan) ExecuteParallel(ctx context.Context, cli operation.Client) error {
	_, err := p.ExecuteRollout(ctx, cli, RolloutOptions{Parallel: true})
	return err
}

func (p *Plan) serviceBatches() ([][]*deploy.ServicePlan, error) {
	if len(p.Services) == 0 {
		return nil, nil
	}

	planByName := make(map[string]*deploy.ServicePlan, len(p.Services))
	order := make(map[string]int, len(p.Services))
	dependents := make(map[string][]string, len(p.Services))
	inDegree := make(map[string]int, len(p.Services))
	for i, sp := range p.Services {
		planByName[sp.ServiceName] = sp
		order[sp.ServiceName] = i
		inDegree[sp.ServiceName] = 0
	}

	for serviceName, deps := range p.serviceDependencies {
		if _, ok := planByName[serviceName]; !ok {
			continue
		}
		for _, depName := range deps {
			if _, ok := planByName[depName]; !ok {
				return nil, fmt.Errorf("service '%s' depends on missing planned service '%s'", serviceName, depName)
			}
			inDegree[serviceName]++
			dependents[depName] = append(dependents[depName], serviceName)
		}
	}

	ready := make([]string, 0, len(p.Services))
	for _, sp := range p.Services {
		if inDegree[sp.ServiceName] == 0 {
			ready = append(ready, sp.ServiceName)
		}
	}

	var (
		batches   [][]*deploy.ServicePlan
		processed int
	)
	for len(ready) > 0 {
		batchNames := append([]string(nil), ready...)
		ready = nil

		batch := make([]*deploy.ServicePlan, 0, len(batchNames))
		for _, name := range batchNames {
			batch = append(batch, planByName[name])
			processed++
		}
		batches = append(batches, batch)

		next := make([]string, 0, len(p.Services))
		for _, name := range batchNames {
			for _, dependent := range dependents[name] {
				inDegree[dependent]--
				if inDegree[dependent] == 0 {
					next = append(next, dependent)
				}
			}
		}
		slices.SortFunc(next, func(a, b string) int {
			return order[a] - order[b]
		})
		ready = next
	}

	if processed != len(p.Services) {
		return nil, errors.New("cannot create parallel service batches: dependency cycle detected")
	}
	return batches, nil
}
