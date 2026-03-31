package compose

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stringid"
	"github.com/psviderski/uncloud/internal/machine/api/pb"
	"github.com/psviderski/uncloud/pkg/api"
	"github.com/psviderski/uncloud/pkg/client/deploy"
	"github.com/psviderski/uncloud/pkg/client/deploy/operation"
)

const (
	rolloutCleanupTimeout = 30 * time.Second
	defaultRolloutFanout  = uint64(10)
)

type RolloutOptions struct {
	Parallel bool
}

type ServiceRolloutStatus string

const (
	ServiceRolloutPending           ServiceRolloutStatus = "pending"
	ServiceRolloutRunning           ServiceRolloutStatus = "running"
	ServiceRolloutSucceeded         ServiceRolloutStatus = "succeeded"
	ServiceRolloutRolledBack        ServiceRolloutStatus = "rolled_back"
	ServiceRolloutFailedDirty       ServiceRolloutStatus = "failed_dirty"
	ServiceRolloutSkippedDependency ServiceRolloutStatus = "skipped_dependency"
)

type ServiceRolloutResult struct {
	ServiceName string
	Status      ServiceRolloutStatus
	Err         error
}

type RolloutResult struct {
	Services []ServiceRolloutResult
}

func (r RolloutResult) HasFailures() bool {
	return slices.ContainsFunc(r.Services, func(s ServiceRolloutResult) bool {
		return s.Status == ServiceRolloutRolledBack || s.Status == ServiceRolloutFailedDirty
	})
}

func (r RolloutResult) Error() error {
	var errs []error
	for _, service := range r.Services {
		switch service.Status {
		case ServiceRolloutRolledBack, ServiceRolloutFailedDirty:
			if service.Err != nil {
				errs = append(errs, fmt.Errorf("service '%s' %s: %w", service.ServiceName, service.Status, service.Err))
				continue
			}
			errs = append(errs, fmt.Errorf("service '%s' %s", service.ServiceName, service.Status))
		}
	}
	return errors.Join(errs...)
}

func (r RolloutResult) Format() string {
	if len(r.Services) == 0 {
		return ""
	}

	var out strings.Builder
	out.WriteString("Deployment result\n\n")
	for _, service := range r.Services {
		out.WriteString("- ")
		out.WriteString(service.ServiceName)
		out.WriteString(": ")
		out.WriteString(string(service.Status))
		if service.Err != nil {
			out.WriteString(" (")
			out.WriteString(service.Err.Error())
			out.WriteString(")")
		}
		out.WriteString("\n")
	}
	return out.String()
}

func (p *Plan) ExecuteRollout(ctx context.Context, cli operation.Client, opts RolloutOptions) (RolloutResult, error) {
	for _, op := range p.Volumes {
		if err := op.Execute(ctx, cli); err != nil {
			return RolloutResult{}, err
		}
	}

	batches, err := p.serviceBatches()
	if err != nil {
		return RolloutResult{}, err
	}

	servicesByName := make(map[string]serviceExecution, len(p.Services))
	for _, sp := range p.Services {
		servicesByName[sp.ServiceName] = serviceExecution{
			ServiceName: sp.ServiceName,
			Status:      ServiceRolloutPending,
		}
	}

	abort := false
	for _, batch := range batches {
		if abort {
			for _, sp := range batch {
				result := servicesByName[sp.ServiceName]
				result.Status = ServiceRolloutSkippedDependency
				servicesByName[sp.ServiceName] = result
			}
			continue
		}

		bootstrap := executeBootstrapBatch(ctx, cli, batch, opts.Parallel)
		bootstrapFailed := false
		for _, result := range bootstrap {
			if result.state.Status != ServiceRolloutPending {
				servicesByName[result.state.ServiceName] = result.state
			}
			if result.ran && result.state.Status != ServiceRolloutSucceeded {
				bootstrapFailed = true
			}
		}
		if bootstrapFailed {
			rollbackSuccessfulBootstrap(cli, bootstrap, &servicesByName)
			for _, result := range bootstrap {
				if !result.ran {
					current := servicesByName[result.state.ServiceName]
					current.Status = ServiceRolloutSkippedDependency
					servicesByName[result.state.ServiceName] = current
				}
			}
			abort = true
			continue
		}

		tasks := make([]serviceTask, 0, len(batch))
		for _, result := range bootstrap {
			if len(result.remainder) == 0 {
				if result.state.Status == ServiceRolloutPending {
					result.state.Status = ServiceRolloutSucceeded
					result.state.completedAt = time.Now()
					servicesByName[result.state.ServiceName] = result.state
				}
				continue
			}
			tasks = append(tasks, serviceTask{
				serviceName: result.state.ServiceName,
				initial:     result.state,
				operations:  result.remainder,
				parallelism: resolveRolloutParallelism(findServicePlan(batch, result.state.ServiceName)),
			})
		}

		remainder := executeServiceTasks(ctx, cli, tasks, opts.Parallel)
		frontierFailed := false
		for _, result := range remainder {
			servicesByName[result.ServiceName] = result
			if result.Status != ServiceRolloutSucceeded {
				frontierFailed = true
			}
		}
		if frontierFailed {
			abort = true
		}
	}

	result := RolloutResult{
		Services: make([]ServiceRolloutResult, 0, len(p.Services)),
	}
	for _, sp := range p.Services {
		service := servicesByName[sp.ServiceName]
		result.Services = append(result.Services, ServiceRolloutResult{
			ServiceName: service.ServiceName,
			Status:      service.Status,
			Err:         service.Err,
		})
	}

	return result, result.Error()
}

type containerRef struct {
	ID   string
	Name string
}

type rollbackEntry struct {
	run func(context.Context, operation.Client) error
}

type serviceExecution struct {
	ServiceName            string
	Status                 ServiceRolloutStatus
	Err                    error
	completedAt            time.Time
	rollbackEntries        []rollbackEntry
	hasIrreversibleSuccess bool
}

type serviceTask struct {
	serviceName string
	initial     serviceExecution
	operations  []operation.Operation
	parallelism uint64
}

type bootstrapExecution struct {
	state     serviceExecution
	remainder []operation.Operation
	ran       bool
}

type rolloutOperationKind uint8

const (
	rolloutOperationBarrier rolloutOperationKind = iota
	rolloutOperationRun
	rolloutOperationReplaceStartFirst
	rolloutOperationReplaceStopFirst
)

type trackedOperation interface {
	executeForRollout(context.Context, operation.Client) operationExecution
}

type rolloutOperationInfoProvider interface {
	rolloutOperationKind() rolloutOperationKind
}

type operationExecution struct {
	err                 error
	rollback            *rollbackEntry
	replaceLastRollback bool
	irreversible        bool
	safeFailure         bool
}

type finalizeReplaceOperation struct {
	op     *operation.ReplaceContainerOperation
	newCtr containerRef
}

func (o *finalizeReplaceOperation) Execute(ctx context.Context, cli operation.Client) error {
	return o.executeForRollout(ctx, cli).err
}

func (o *finalizeReplaceOperation) Format() string {
	return "finalize replace container"
}

func (o *finalizeReplaceOperation) String() string {
	return fmt.Sprintf("FinalizeReplaceOperation[service_id=%s old_container_id=%s order=%s]",
		o.op.ServiceID, o.op.OldContainer.ID, o.op.Order)
}

func (o *finalizeReplaceOperation) executeForRollout(ctx context.Context, cli operation.Client) operationExecution {
	stopFirst := o.op.Order == api.UpdateOrderStopFirst

	if !stopFirst && o.op.OldContainer.State.Running {
		if err := cli.StopContainerOnMachine(ctx, o.op.Machine, o.op.OldContainer.ID, o.op.OldContainer.Name, containerStopOptions(o.op.StopGracePeriod)); err != nil {
			return operationExecution{
				err:         fmt.Errorf("stop old container: %w", err),
				safeFailure: true,
			}
		}
	}

	if err := cli.RemoveContainerOnMachine(ctx, o.op.Machine, o.op.OldContainer.ID, o.op.OldContainer.Name, container.RemoveOptions{
		RemoveVolumes: true,
	}); err != nil {
		if !stopFirst && o.op.OldContainer.State.Running {
			rollbackErr := restartOldContainer(cli, o.op)
			return operationExecution{
				err:         errors.Join(fmt.Errorf("remove old container: %w", err), rollbackErr),
				safeFailure: rollbackErr == nil,
			}
		}
		return operationExecution{
			err:         fmt.Errorf("remove old container: %w", err),
			safeFailure: true,
		}
	}

	rollback := rollbackEntry{
		run: func(ctx context.Context, cli operation.Client) error {
			return rollbackReplaceOperation(ctx, cli, o.op, o.newCtr)
		},
	}
	return operationExecution{
		rollback:            &rollback,
		replaceLastRollback: true,
	}
}

func executeBootstrapBatch(
	ctx context.Context, cli operation.Client, batch []*deploy.ServicePlan, parallel bool,
) []bootstrapExecution {
	if len(batch) == 0 {
		return nil
	}
	if !parallel {
		results := make([]bootstrapExecution, 0, len(batch))
		for _, sp := range batch {
			results = append(results, executeBootstrapService(ctx, cli, sp))
		}
		return results
	}

	results := make([]bootstrapExecution, len(batch))
	var wg sync.WaitGroup
	for i, sp := range batch {
		wg.Add(1)
		go func(i int, sp *deploy.ServicePlan) {
			defer wg.Done()
			results[i] = executeBootstrapService(ctx, cli, sp)
		}(i, sp)
	}
	wg.Wait()
	return results
}

func executeBootstrapService(ctx context.Context, cli operation.Client, sp *deploy.ServicePlan) bootstrapExecution {
	state := serviceExecution{
		ServiceName: sp.ServiceName,
		Status:      ServiceRolloutPending,
	}
	if len(sp.Operations) == 0 {
		return bootstrapExecution{state: state}
	}

	canaryIndex := firstCanaryOperationIndex(sp)
	if canaryIndex < 0 {
		return bootstrapExecution{
			state:     state,
			remainder: sp.Operations,
		}
	}

	if canaryIndex > 0 {
		state = executeServiceOperations(ctx, cli, state, sp.Operations[:canaryIndex])
		if state.Status != ServiceRolloutSucceeded {
			return bootstrapExecution{
				state: state,
				ran:   true,
			}
		}
	}

	switch op := sp.Operations[canaryIndex].(type) {
	case *operation.ReplaceContainerOperation:
		result, finalize := executeBootstrapReplaceOperation(ctx, cli, op, state)
		remainder := sp.Operations[canaryIndex+1:]
		if finalize != nil {
			remainder = append([]operation.Operation{finalize}, remainder...)
		}
		return bootstrapExecution{
			state:     result,
			remainder: remainder,
			ran:       true,
		}
	default:
		return bootstrapExecution{
			state:     executeServiceOperations(ctx, cli, state, sp.Operations[:canaryIndex+1]),
			remainder: sp.Operations[canaryIndex+1:],
			ran:       true,
		}
	}
}

func executeBootstrapReplaceOperation(
	ctx context.Context,
	cli operation.Client,
	op *operation.ReplaceContainerOperation,
	state serviceExecution,
) (serviceExecution, operation.Operation) {
	state.Status = ServiceRolloutRunning
	stopFirst := op.Order == api.UpdateOrderStopFirst
	wasRunning := op.OldContainer.State.Running

	if stopFirst && wasRunning {
		if err := cli.StopContainerOnMachine(ctx, op.Machine, op.OldContainer.ID, op.OldContainer.Name, containerStopOptions(op.StopGracePeriod)); err != nil {
			state.Status = ServiceRolloutFailedDirty
			state.completedAt = time.Now()
			state.Err = fmt.Errorf("stop old container: %w", err)
			return state, nil
		}
	}

	resp, containerName, err := cli.CreateContainerOnMachine(ctx, op.ServiceID, op.Spec, op.Machine)
	if err != nil {
		if stopFirst && wasRunning {
			rollbackErr := restartOldContainer(cli, op)
			state.Status = serviceFailureStatus(false, rollbackErr)
			state.Err = errors.Join(fmt.Errorf("create new container: %w", err), rollbackErr)
			state.completedAt = time.Now()
			return state, nil
		}
		state.Status = ServiceRolloutRolledBack
		state.Err = fmt.Errorf("create new container: %w", err)
		state.completedAt = time.Now()
		return state, nil
	}
	newCtr := containerRef{ID: resp.ID, Name: containerName}

	if err = cli.StartContainerOnMachine(ctx, op.Machine, newCtr.ID, newCtr.Name); err != nil {
		cleanupErr := cleanupCreatedContainer(cli, op.Machine, newCtr)
		if stopFirst && wasRunning {
			rollbackErr := restartOldContainer(cli, op)
			state.Status = serviceFailureStatus(false, errors.Join(cleanupErr, rollbackErr))
			state.Err = errors.Join(fmt.Errorf("start new container: %w", err), cleanupErr, rollbackErr)
			state.completedAt = time.Now()
			return state, nil
		}
		state.Status = serviceFailureStatus(true, cleanupErr)
		state.Err = errors.Join(fmt.Errorf("start new container: %w", err), cleanupErr)
		state.completedAt = time.Now()
		return state, nil
	}

	if !op.SkipHealthMonitor {
		opts := api.WaitContainerHealthyOptions{MonitorPeriod: op.Spec.UpdateConfig.MonitorPeriod}
		if err = cli.WaitContainerHealthyOnMachine(ctx, op.Machine, newCtr.ID, newCtr.Name, opts); err != nil {
			inspectErr := stopContainerForInspection(cli, op.Machine, newCtr, op.StopGracePeriod)
			healthErr := fmt.Errorf("new container '%s/%s' failed to become healthy: %w",
				op.Spec.Name, stringid.TruncateID(newCtr.ID), err)
			if stopFirst && wasRunning {
				rollbackErr := restartOldContainer(cli, op)
				state.Status = serviceFailureStatus(false, errors.Join(inspectErr, rollbackErr))
				state.Err = errors.Join(healthErr, inspectErr, rollbackErr)
				state.completedAt = time.Now()
				return state, nil
			}
			state.Status = serviceFailureStatus(true, inspectErr)
			state.Err = errors.Join(healthErr, inspectErr)
			state.completedAt = time.Now()
			return state, nil
		}
	}

	rollback := rollbackEntry{
		run: func(ctx context.Context, cli operation.Client) error {
			if stopFirst {
				if err := removeContainer(ctx, cli, op.Machine, newCtr, container.RemoveOptions{
					Force:         true,
					RemoveVolumes: true,
				}); err != nil {
					return err
				}
				if wasRunning {
					return cli.StartContainerOnMachine(ctx, op.Machine, op.OldContainer.ID, op.OldContainer.Name)
				}
				return nil
			}
			return removeContainer(ctx, cli, op.Machine, newCtr, container.RemoveOptions{
				Force:         true,
				RemoveVolumes: true,
			})
		},
	}
	state.rollbackEntries = append(state.rollbackEntries, rollback)
	state.Status = ServiceRolloutSucceeded
	state.completedAt = time.Now()
	return state, &finalizeReplaceOperation{op: op, newCtr: newCtr}
}

func executeServiceTasks(ctx context.Context, cli operation.Client, tasks []serviceTask, parallel bool) []serviceExecution {
	if len(tasks) == 0 {
		return nil
	}
	if !parallel {
		results := make([]serviceExecution, 0, len(tasks))
		for _, task := range tasks {
			results = append(results, executeServiceTask(ctx, cli, task))
		}
		return results
	}

	results := make([]serviceExecution, len(tasks))
	var wg sync.WaitGroup
	for i, task := range tasks {
		wg.Add(1)
		go func(i int, task serviceTask) {
			defer wg.Done()
			results[i] = executeServiceTask(ctx, cli, task)
		}(i, task)
	}
	wg.Wait()
	return results
}

func executeServiceTask(ctx context.Context, cli operation.Client, task serviceTask) serviceExecution {
	result := task.initial
	if result.Status != ServiceRolloutPending && result.Status != ServiceRolloutSucceeded {
		return result
	}
	if len(task.operations) == 0 {
		if result.Status == ServiceRolloutPending {
			result.Status = ServiceRolloutSucceeded
			result.completedAt = time.Now()
		}
		return result
	}

	ops := task.operations
	for len(ops) > 0 {
		if !isParallelizableRolloutOperation(ops[0]) || task.parallelism <= 1 {
			result = executeServiceOperations(ctx, cli, result, ops[:1])
			if result.Status != ServiceRolloutSucceeded {
				return result
			}
			ops = ops[1:]
			continue
		}

		groupLen := 0
		for groupLen < len(ops) && isParallelizableRolloutOperation(ops[groupLen]) {
			groupLen++
		}
		group := ops[:groupLen]
		ops = ops[groupLen:]

		for len(group) > 0 {
			result = executeServiceOperationGroup(ctx, cli, result, group, task.parallelism)
			if result.Status != ServiceRolloutSucceeded {
				return result
			}
			group = nil
		}
	}

	result.Status = ServiceRolloutSucceeded
	result.completedAt = time.Now()
	return result
}

func executeServiceOperations(
	ctx context.Context,
	cli operation.Client,
	initial serviceExecution,
	ops []operation.Operation,
) serviceExecution {
	result := initial
	if result.Status != ServiceRolloutPending && result.Status != ServiceRolloutSucceeded {
		return result
	}
	if len(ops) == 0 {
		if result.Status == ServiceRolloutPending {
			result.Status = ServiceRolloutSucceeded
			result.completedAt = time.Now()
		}
		return result
	}

	result.Status = ServiceRolloutRunning
	for _, raw := range ops {
		exec := executeRolloutOperation(ctx, cli, raw)
		if exec.rollback != nil {
			if exec.replaceLastRollback && len(result.rollbackEntries) > 0 {
				result.rollbackEntries[len(result.rollbackEntries)-1] = *exec.rollback
			} else {
				result.rollbackEntries = append(result.rollbackEntries, *exec.rollback)
			}
		}
		if exec.irreversible {
			result.hasIrreversibleSuccess = true
		}
		if exec.err == nil {
			continue
		}

		rollbackErr := rollbackServiceEntries(cli, result.rollbackEntries)
		result.completedAt = time.Now()
		switch {
		case result.hasIrreversibleSuccess:
			result.Status = ServiceRolloutFailedDirty
		case !exec.safeFailure:
			result.Status = ServiceRolloutFailedDirty
		case rollbackErr != nil:
			result.Status = ServiceRolloutFailedDirty
		default:
			result.Status = ServiceRolloutRolledBack
		}
		result.Err = errors.Join(exec.err, rollbackErr)
		return result
	}

	result.Status = ServiceRolloutSucceeded
	result.completedAt = time.Now()
	return result
}

func executeServiceOperationGroup(
	ctx context.Context,
	cli operation.Client,
	initial serviceExecution,
	ops []operation.Operation,
	parallelism uint64,
) serviceExecution {
	result := initial
	if len(ops) == 0 {
		return result
	}

	maxWorkers := int(parallelism)
	if maxWorkers <= 0 || maxWorkers > len(ops) {
		maxWorkers = len(ops)
	}

	type executionResult struct {
		index int
		exec  operationExecution
	}
	results := make(chan executionResult, maxWorkers)
	executions := make([]operationExecution, len(ops))

	next := 0
	inFlight := 0
	stopScheduling := false
	start := func(index int) {
		inFlight++
		go func(index int, raw operation.Operation) {
			results <- executionResult{
				index: index,
				exec:  executeRolloutOperation(ctx, cli, raw),
			}
		}(index, ops[index])
	}

	for next < len(ops) && inFlight < maxWorkers {
		start(next)
		next++
	}

	launched := next
	for inFlight > 0 {
		execution := <-results
		inFlight--
		executions[execution.index] = execution.exec
		if execution.exec.err != nil {
			stopScheduling = true
		}

		if stopScheduling {
			continue
		}
		if next >= len(ops) {
			continue
		}
		start(next)
		next++
		launched = next
	}

	for _, exec := range executions[:launched] {
		if exec.rollback != nil {
			if exec.replaceLastRollback && len(result.rollbackEntries) > 0 {
				result.rollbackEntries[len(result.rollbackEntries)-1] = *exec.rollback
			} else {
				result.rollbackEntries = append(result.rollbackEntries, *exec.rollback)
			}
		}
		if exec.irreversible {
			result.hasIrreversibleSuccess = true
		}
	}

	var firstErr error
	var safeFailure = true
	for _, exec := range executions[:launched] {
		if exec.err == nil {
			continue
		}
		if firstErr == nil {
			firstErr = exec.err
		}
		if !exec.safeFailure {
			safeFailure = false
		}
	}
	if firstErr == nil {
		result.Status = ServiceRolloutSucceeded
		result.completedAt = time.Now()
		return result
	}

	rollbackErr := rollbackServiceEntries(cli, result.rollbackEntries)
	result.completedAt = time.Now()
	switch {
	case result.hasIrreversibleSuccess:
		result.Status = ServiceRolloutFailedDirty
	case !safeFailure:
		result.Status = ServiceRolloutFailedDirty
	case rollbackErr != nil:
		result.Status = ServiceRolloutFailedDirty
	default:
		result.Status = ServiceRolloutRolledBack
	}
	var errs []error
	for _, exec := range executions[:launched] {
		if exec.err != nil {
			errs = append(errs, exec.err)
		}
	}
	errs = append(errs, rollbackErr)
	result.Err = errors.Join(errs...)
	return result
}

func executeRolloutOperation(ctx context.Context, cli operation.Client, raw operation.Operation) operationExecution {
	if op, ok := raw.(trackedOperation); ok {
		return op.executeForRollout(ctx, cli)
	}

	switch op := raw.(type) {
	case *operation.RunContainerOperation:
		return executeRunOperation(ctx, cli, op)
	case *operation.ReplaceContainerOperation:
		return executeReplaceOperation(ctx, cli, op)
	case *operation.RemoveContainerOperation, *operation.StopContainerOperation:
		if err := raw.Execute(ctx, cli); err != nil {
			return operationExecution{err: err}
		}
		return operationExecution{irreversible: true}
	default:
		if err := raw.Execute(ctx, cli); err != nil {
			return operationExecution{err: err}
		}
		return operationExecution{irreversible: true}
	}
}

func executeRunOperation(
	ctx context.Context, cli operation.Client, op *operation.RunContainerOperation,
) operationExecution {
	resp, containerName, err := cli.CreateContainerOnMachine(ctx, op.ServiceID, op.Spec, op.Machine)
	if err != nil {
		return operationExecution{err: fmt.Errorf("create container: %w", err), safeFailure: true}
	}
	ctr := containerRef{ID: resp.ID, Name: containerName}

	if err = cli.StartContainerOnMachine(ctx, op.Machine, ctr.ID, ctr.Name); err != nil {
		cleanupErr := cleanupCreatedContainer(cli, op.Machine, ctr)
		return operationExecution{
			err:         errors.Join(fmt.Errorf("start container: %w", err), cleanupErr),
			safeFailure: cleanupErr == nil,
		}
	}

	if !op.SkipHealthMonitor {
		opts := api.WaitContainerHealthyOptions{MonitorPeriod: op.Spec.UpdateConfig.MonitorPeriod}
		if err = cli.WaitContainerHealthyOnMachine(ctx, op.Machine, ctr.ID, ctr.Name, opts); err != nil {
			inspectErr := stopContainerForInspection(cli, op.Machine, ctr, op.Spec.StopGracePeriod)
			return operationExecution{
				err: errors.Join(
					fmt.Errorf("container '%s/%s' failed to become healthy: %w", op.Spec.Name, stringid.TruncateID(ctr.ID), err),
					inspectErr,
				),
				safeFailure: inspectErr == nil,
			}
		}
	}

	rollback := rollbackEntry{
		run: func(ctx context.Context, cli operation.Client) error {
			return removeContainer(ctx, cli, op.Machine, ctr, container.RemoveOptions{
				Force:         true,
				RemoveVolumes: true,
			})
		},
	}
	return operationExecution{rollback: &rollback}
}

func executeReplaceOperation(
	ctx context.Context, cli operation.Client, op *operation.ReplaceContainerOperation,
) operationExecution {
	stopFirst := op.Order == api.UpdateOrderStopFirst
	wasRunning := op.OldContainer.State.Running

	if stopFirst && wasRunning {
		if err := cli.StopContainerOnMachine(ctx, op.Machine, op.OldContainer.ID, op.OldContainer.Name, containerStopOptions(op.StopGracePeriod)); err != nil {
			return operationExecution{err: fmt.Errorf("stop old container: %w", err)}
		}
	}

	resp, containerName, err := cli.CreateContainerOnMachine(ctx, op.ServiceID, op.Spec, op.Machine)
	if err != nil {
		if stopFirst && wasRunning {
			rollbackErr := restartOldContainer(cli, op)
			return operationExecution{
				err:         errors.Join(fmt.Errorf("create new container: %w", err), rollbackErr),
				safeFailure: rollbackErr == nil,
			}
		}
		return operationExecution{err: fmt.Errorf("create new container: %w", err), safeFailure: true}
	}
	newCtr := containerRef{ID: resp.ID, Name: containerName}

	if err = cli.StartContainerOnMachine(ctx, op.Machine, newCtr.ID, newCtr.Name); err != nil {
		cleanupErr := cleanupCreatedContainer(cli, op.Machine, newCtr)
		if stopFirst && wasRunning {
			rollbackErr := restartOldContainer(cli, op)
			return operationExecution{
				err:         errors.Join(fmt.Errorf("start new container: %w", err), cleanupErr, rollbackErr),
				safeFailure: cleanupErr == nil && rollbackErr == nil,
			}
		}
		return operationExecution{
			err:         errors.Join(fmt.Errorf("start new container: %w", err), cleanupErr),
			safeFailure: cleanupErr == nil,
		}
	}

	if !op.SkipHealthMonitor {
		opts := api.WaitContainerHealthyOptions{MonitorPeriod: op.Spec.UpdateConfig.MonitorPeriod}
		if err = cli.WaitContainerHealthyOnMachine(ctx, op.Machine, newCtr.ID, newCtr.Name, opts); err != nil {
			inspectErr := stopContainerForInspection(cli, op.Machine, newCtr, op.StopGracePeriod)
			healthErr := fmt.Errorf("new container '%s/%s' failed to become healthy: %w",
				op.Spec.Name, stringid.TruncateID(newCtr.ID), err)
			if stopFirst && wasRunning {
				rollbackErr := restartOldContainer(cli, op)
				return operationExecution{
					err:         errors.Join(healthErr, inspectErr, rollbackErr),
					safeFailure: inspectErr == nil && rollbackErr == nil,
				}
			}
			return operationExecution{
				err:         errors.Join(healthErr, inspectErr),
				safeFailure: inspectErr == nil,
			}
		}
	}

	if !stopFirst && wasRunning {
		if err = cli.StopContainerOnMachine(ctx, op.Machine, op.OldContainer.ID, op.OldContainer.Name, containerStopOptions(op.StopGracePeriod)); err != nil {
			return operationExecution{err: fmt.Errorf("stop old container: %w", err)}
		}
	}

	if err = cli.RemoveContainerOnMachine(ctx, op.Machine, op.OldContainer.ID, op.OldContainer.Name, container.RemoveOptions{
		RemoveVolumes: true,
	}); err != nil {
		if !stopFirst && wasRunning {
			rollbackErr := restartOldContainer(cli, op)
			return operationExecution{
				err:         errors.Join(fmt.Errorf("remove old container: %w", err), rollbackErr),
				safeFailure: rollbackErr == nil,
			}
		}
		return operationExecution{err: fmt.Errorf("remove old container: %w", err)}
	}

	rollback := rollbackEntry{
		run: func(ctx context.Context, cli operation.Client) error {
			return rollbackReplaceOperation(ctx, cli, op, newCtr)
		},
	}
	return operationExecution{rollback: &rollback}
}

func rollbackSuccessfulBootstrap(
	cli operation.Client,
	bootstrap []bootstrapExecution,
	results *map[string]serviceExecution,
) {
	successes := make([]serviceExecution, 0, len(bootstrap))
	for _, result := range bootstrap {
		if result.state.Status == ServiceRolloutSucceeded {
			successes = append(successes, result.state)
		}
	}

	slices.SortFunc(successes, func(a, b serviceExecution) int {
		return b.completedAt.Compare(a.completedAt)
	})

	for _, service := range successes {
		current := (*results)[service.ServiceName]
		if current.hasIrreversibleSuccess {
			current.Status = ServiceRolloutFailedDirty
			current.Err = errors.Join(current.Err, fmt.Errorf("service has committed non-rollback-safe operations"))
			(*results)[service.ServiceName] = current
			continue
		}

		if err := rollbackServiceEntries(cli, current.rollbackEntries); err != nil {
			current.Status = ServiceRolloutFailedDirty
			current.Err = errors.Join(current.Err, err)
		} else {
			current.Status = ServiceRolloutRolledBack
		}
		(*results)[service.ServiceName] = current
	}
}

func findServicePlan(batch []*deploy.ServicePlan, serviceName string) *deploy.ServicePlan {
	for _, sp := range batch {
		if sp.ServiceName == serviceName {
			return sp
		}
	}
	return nil
}

func firstCanaryOperationIndex(sp *deploy.ServicePlan) int {
	for i, raw := range sp.Operations {
		if isCanaryOperation(sp, raw) {
			return i
		}
	}
	return -1
}

func isCanaryOperation(sp *deploy.ServicePlan, raw operation.Operation) bool {
	switch rolloutKindForOperation(raw) {
	case rolloutOperationReplaceStartFirst, rolloutOperationReplaceStopFirst:
		return true
	case rolloutOperationRun:
		return sp.IsNewService
	default:
		return false
	}
}

func isParallelizableRolloutOperation(raw operation.Operation) bool {
	switch rolloutKindForOperation(raw) {
	case rolloutOperationRun, rolloutOperationReplaceStartFirst:
		return true
	default:
		return false
	}
}

func rolloutKindForOperation(raw operation.Operation) rolloutOperationKind {
	if provider, ok := raw.(rolloutOperationInfoProvider); ok {
		return provider.rolloutOperationKind()
	}

	switch op := raw.(type) {
	case *operation.RunContainerOperation:
		return rolloutOperationRun
	case *operation.ReplaceContainerOperation:
		if op.Order == api.UpdateOrderStopFirst {
			return rolloutOperationReplaceStopFirst
		}
		return rolloutOperationReplaceStartFirst
	default:
		return rolloutOperationBarrier
	}
}

func resolveRolloutParallelism(sp *deploy.ServicePlan) uint64 {
	if sp == nil {
		return 1
	}
	if sp.Spec.UpdateConfig.Parallelism != nil && *sp.Spec.UpdateConfig.Parallelism > 0 {
		return *sp.Spec.UpdateConfig.Parallelism
	}
	return defaultRolloutFanout
}

func rollbackServiceEntries(cli operation.Client, entries []rollbackEntry) error {
	var err error
	for i := len(entries) - 1; i >= 0; i-- {
		ctx, cancel := detachedCleanupContext()
		err = errors.Join(err, entries[i].run(ctx, cli))
		cancel()
	}
	return err
}

func cleanupCreatedContainer(cli operation.Client, machine *pb.MachineInfo, ctr containerRef) error {
	ctx, cancel := detachedCleanupContext()
	defer cancel()
	return removeContainer(ctx, cli, machine, ctr, container.RemoveOptions{
		Force:         true,
		RemoveVolumes: true,
	})
}

func stopContainerForInspection(
	cli operation.Client,
	machine *pb.MachineInfo,
	ctr containerRef,
	gracePeriod *time.Duration,
) error {
	ctx, cancel := detachedCleanupContext()
	defer cancel()
	if err := cli.StopContainerOnMachine(ctx, machine, ctr.ID, ctr.Name, containerStopOptions(gracePeriod)); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	return nil
}

func restartOldContainer(cli operation.Client, op *operation.ReplaceContainerOperation) error {
	ctx, cancel := detachedCleanupContext()
	defer cancel()
	return cli.StartContainerOnMachine(ctx, op.Machine, op.OldContainer.ID, op.OldContainer.Name)
}

func rollbackReplaceOperation(
	ctx context.Context,
	cli operation.Client,
	op *operation.ReplaceContainerOperation,
	current containerRef,
) error {
	oldSpec := op.OldContainer.ServiceSpec.Clone()
	if oldSpec.UpdateConfig.Order == "" {
		oldSpec.UpdateConfig.Order = op.Order
	}

	if op.Order == api.UpdateOrderStopFirst {
		if err := removeContainer(ctx, cli, op.Machine, current, container.RemoveOptions{
			Force:         true,
			RemoveVolumes: true,
		}); err != nil {
			return err
		}
		return createAndStartRollbackTarget(ctx, cli, op.ServiceID, op.Machine, oldSpec)
	}

	resp, containerName, err := cli.CreateContainerOnMachine(ctx, op.ServiceID, oldSpec, op.Machine)
	if err != nil {
		return fmt.Errorf("create old container for rollback: %w", err)
	}
	oldCtr := containerRef{ID: resp.ID, Name: containerName}
	if err = cli.StartContainerOnMachine(ctx, op.Machine, oldCtr.ID, oldCtr.Name); err != nil {
		cleanupErr := removeContainer(ctx, cli, op.Machine, oldCtr, container.RemoveOptions{
			Force:         true,
			RemoveVolumes: true,
		})
		return errors.Join(fmt.Errorf("start old container for rollback: %w", err), cleanupErr)
	}

	opts := api.WaitContainerHealthyOptions{MonitorPeriod: oldSpec.UpdateConfig.MonitorPeriod}
	if err = cli.WaitContainerHealthyOnMachine(ctx, op.Machine, oldCtr.ID, oldCtr.Name, opts); err != nil {
		cleanupErr := removeContainer(ctx, cli, op.Machine, oldCtr, container.RemoveOptions{
			Force:         true,
			RemoveVolumes: true,
		})
		return errors.Join(fmt.Errorf("old container failed to become healthy during rollback: %w", err), cleanupErr)
	}

	return removeContainer(ctx, cli, op.Machine, current, container.RemoveOptions{
		Force:         true,
		RemoveVolumes: true,
	})
}

func createAndStartRollbackTarget(
	ctx context.Context,
	cli operation.Client,
	serviceID string,
	machine *pb.MachineInfo,
	spec api.ServiceSpec,
) error {
	resp, containerName, err := cli.CreateContainerOnMachine(ctx, serviceID, spec, machine)
	if err != nil {
		return fmt.Errorf("create old container for rollback: %w", err)
	}
	oldCtr := containerRef{ID: resp.ID, Name: containerName}
	if err = cli.StartContainerOnMachine(ctx, machine, oldCtr.ID, oldCtr.Name); err != nil {
		cleanupErr := removeContainer(ctx, cli, machine, oldCtr, container.RemoveOptions{
			Force:         true,
			RemoveVolumes: true,
		})
		return errors.Join(fmt.Errorf("start old container for rollback: %w", err), cleanupErr)
	}

	opts := api.WaitContainerHealthyOptions{MonitorPeriod: spec.UpdateConfig.MonitorPeriod}
	if err = cli.WaitContainerHealthyOnMachine(ctx, machine, oldCtr.ID, oldCtr.Name, opts); err != nil {
		cleanupErr := removeContainer(ctx, cli, machine, oldCtr, container.RemoveOptions{
			Force:         true,
			RemoveVolumes: true,
		})
		return errors.Join(fmt.Errorf("old container failed to become healthy during rollback: %w", err), cleanupErr)
	}

	return nil
}

func detachedCleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), rolloutCleanupTimeout)
}

func removeContainer(
	ctx context.Context,
	cli operation.Client,
	machine *pb.MachineInfo,
	ctr containerRef,
	opts container.RemoveOptions,
) error {
	err := cli.RemoveContainerOnMachine(ctx, machine, ctr.ID, ctr.Name, opts)
	if err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	return nil
}

func containerStopOptions(gracePeriod *time.Duration) container.StopOptions {
	if gracePeriod == nil {
		return container.StopOptions{}
	}
	timeout := int(gracePeriod.Seconds())
	return container.StopOptions{Timeout: &timeout}
}

func serviceFailureStatus(safeFailure bool, rollbackErr error) ServiceRolloutStatus {
	if safeFailure && rollbackErr == nil {
		return ServiceRolloutRolledBack
	}
	return ServiceRolloutFailedDirty
}
