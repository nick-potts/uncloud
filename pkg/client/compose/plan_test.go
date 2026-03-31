package compose

import (
	"context"
	"errors"
	"sync"

	"github.com/psviderski/uncloud/pkg/api"
	"github.com/psviderski/uncloud/pkg/client/deploy"
	"github.com/psviderski/uncloud/pkg/client/deploy/operation"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestPlanServiceBatches(t *testing.T) {
	plan := Plan{
		Services: []*deploy.ServicePlan{
			newServicePlan("db"),
			newServicePlan("api"),
			newServicePlan("worker"),
			newServicePlan("web"),
		},
		serviceDependencies: map[string][]string{
			"api":    {"db"},
			"worker": {"db"},
			"web":    {"api"},
		},
	}

	batches, err := plan.serviceBatches()
	require.NoError(t, err)
	require.Len(t, batches, 3)

	assert.Equal(t, []string{"db"}, batchNames(batches[0]))
	assert.Equal(t, []string{"api", "worker"}, batchNames(batches[1]))
	assert.Equal(t, []string{"web"}, batchNames(batches[2]))
}

func TestPlanExecuteParallelSkipsDependentBatchesAfterFailure(t *testing.T) {
	var dependentExecuted bool
	plan := Plan{
		Services: []*deploy.ServicePlan{
			newServicePlan("db", fakeRolloutOperation{
				executeForRolloutFn: func(context.Context, operation.Client) operationExecution {
					return operationExecution{err: errors.New("boom")}
				},
			}),
			newServicePlan("web", fakeRolloutOperation{
				executeForRolloutFn: func(context.Context, operation.Client) operationExecution {
					dependentExecuted = true
					return operationExecution{}
				},
			}),
		},
		serviceDependencies: map[string][]string{
			"web": {"db"},
		},
	}

	err := plan.ExecuteParallel(context.Background(), nil)
	require.Error(t, err)
	assert.ErrorContains(t, err, "service 'db' failed_dirty")
	assert.False(t, dependentExecuted)
}

func TestPlanExecuteRolloutBootstrapFailureRollsBackSuccessfulCanariesOnly(t *testing.T) {
	var (
		mu         sync.Mutex
		rolledBack []string
	)
	recordRollback := func(name string) {
		mu.Lock()
		defer mu.Unlock()
		rolledBack = append(rolledBack, name)
	}

	plan := Plan{
		Services: []*deploy.ServicePlan{
			newServicePlan("db", fakeRolloutOperation{
				executeForRolloutFn: func(context.Context, operation.Client) operationExecution {
					return operationExecution{
						rollback: &rollbackEntry{
							run: func(context.Context, operation.Client) error {
								recordRollback("db")
								return nil
							},
						},
					}
				},
			}),
			newServicePlan("api", fakeRolloutOperation{
				executeForRolloutFn: func(context.Context, operation.Client) operationExecution {
					return operationExecution{
						rollback: &rollbackEntry{
							run: func(context.Context, operation.Client) error {
								recordRollback("api")
								return nil
							},
						},
					}
				},
			}),
			newServicePlan("worker", fakeRolloutOperation{
				executeForRolloutFn: func(context.Context, operation.Client) operationExecution {
					return operationExecution{err: errors.New("boom"), safeFailure: true}
				},
			}),
			newServicePlan("web", fakeRolloutOperation{
				executeForRolloutFn: func(context.Context, operation.Client) operationExecution {
					return operationExecution{}
				},
			}),
		},
		serviceDependencies: map[string][]string{
			"api":    {"db"},
			"worker": {"db"},
			"web":    {"api"},
		},
	}
	result, err := plan.ExecuteRollout(context.Background(), nil, RolloutOptions{Parallel: true})
	require.Error(t, err)
	require.Len(t, result.Services, 4)

	assert.Equal(t, ServiceRolloutSucceeded, result.Services[0].Status)
	assert.Equal(t, ServiceRolloutRolledBack, result.Services[1].Status)
	assert.Equal(t, ServiceRolloutRolledBack, result.Services[2].Status)
	assert.Equal(t, ServiceRolloutSkippedDependency, result.Services[3].Status)
	assert.Equal(t, []string{"api"}, rolledBack)
}

func TestPlanExecuteRolloutWaitsForInFlightSiblingBeforeRollback(t *testing.T) {
	var (
		mu    sync.Mutex
		steps []string
	)
	record := func(step string) {
		mu.Lock()
		defer mu.Unlock()
		steps = append(steps, step)
	}

	failDone := make(chan struct{})
	plan := Plan{
		Services: []*deploy.ServicePlan{
			newServicePlan("slow", fakeRolloutOperation{
				useKind: true,
				kind:    rolloutOperationReplaceStartFirst,
				executeForRolloutFn: func(context.Context, operation.Client) operationExecution {
					record("slow:start")
					<-failDone
					record("slow:done")
					return operationExecution{
						rollback: &rollbackEntry{
							run: func(context.Context, operation.Client) error {
								record("slow:rollback")
								return nil
							},
						},
					}
				},
			}),
			newServicePlan("fast", fakeRolloutOperation{
				useKind: true,
				kind:    rolloutOperationReplaceStartFirst,
				executeForRolloutFn: func(context.Context, operation.Client) operationExecution {
					record("fast:fail")
					close(failDone)
					return operationExecution{err: errors.New("boom"), safeFailure: true}
				},
			}),
		},
	}
	_, err := plan.ExecuteRollout(context.Background(), nil, RolloutOptions{Parallel: true})
	require.Error(t, err)
	assert.Contains(t, steps, "slow:start")
	assert.Contains(t, steps, "fast:fail")
	assert.Contains(t, steps, "slow:done")
	assert.Contains(t, steps, "slow:rollback")
	assert.Less(t, indexOf(steps, "slow:start"), indexOf(steps, "slow:done"))
	assert.Less(t, indexOf(steps, "fast:fail"), indexOf(steps, "slow:rollback"))
	assert.Less(t, indexOf(steps, "slow:done"), indexOf(steps, "slow:rollback"))
}

func TestPlanExecuteRolloutScaleUpWaitsForTierCanary(t *testing.T) {
	canaryStarted := make(chan struct{})
	releaseCanary := make(chan struct{})
	scaleStarted := make(chan struct{})
	done := make(chan struct{})
	errCh := make(chan error, 1)

	plan := Plan{
		Services: []*deploy.ServicePlan{
			newServicePlan("api", fakeRolloutOperation{
				useKind: true,
				kind:    rolloutOperationReplaceStartFirst,
				executeForRolloutFn: func(context.Context, operation.Client) operationExecution {
					close(canaryStarted)
					<-releaseCanary
					return operationExecution{}
				},
			}),
			newServicePlan("worker", fakeRolloutOperation{
				useKind: true,
				kind:    rolloutOperationRun,
				executeForRolloutFn: func(context.Context, operation.Client) operationExecution {
					close(scaleStarted)
					return operationExecution{}
				},
			}),
		},
	}

	go func() {
		defer close(done)
		_, err := plan.ExecuteRollout(context.Background(), nil, RolloutOptions{Parallel: true})
		errCh <- err
	}()

	<-canaryStarted
	select {
	case <-scaleStarted:
		t.Fatal("scale-up operation started before tier canary passed")
	default:
	}

	close(releaseCanary)
	<-scaleStarted
	<-done
	require.NoError(t, <-errCh)
}

func TestPlanExecuteRolloutNewServiceRunParticipatesInCanary(t *testing.T) {
	newStarted := make(chan struct{})
	releaseNew := make(chan struct{})
	scaleStarted := make(chan struct{})
	done := make(chan struct{})
	errCh := make(chan error, 1)

	newService := newServicePlan("api", fakeRolloutOperation{
		useKind: true,
		kind:    rolloutOperationRun,
		executeForRolloutFn: func(context.Context, operation.Client) operationExecution {
			close(newStarted)
			<-releaseNew
			return operationExecution{}
		},
	})
	newService.IsNewService = true

	plan := Plan{
		Services: []*deploy.ServicePlan{
			newService,
			newServicePlan("worker", fakeRolloutOperation{
				useKind: true,
				kind:    rolloutOperationRun,
				executeForRolloutFn: func(context.Context, operation.Client) operationExecution {
					close(scaleStarted)
					return operationExecution{}
				},
			}),
		},
	}

	go func() {
		defer close(done)
		_, err := plan.ExecuteRollout(context.Background(), nil, RolloutOptions{Parallel: true})
		errCh <- err
	}()

	<-newStarted
	select {
	case <-scaleStarted:
		t.Fatal("existing service scale-up started before new service canary passed")
	default:
	}

	close(releaseNew)
	<-scaleStarted
	<-done
	require.NoError(t, <-errCh)
}

func TestPlanExecuteRolloutPostCanaryFanoutUsesUpdateParallelism(t *testing.T) {
	canaryStarted := make(chan struct{})
	releaseCanary := make(chan struct{})
	postStarted := make(chan string, 3)
	releasePostOne := make(chan struct{})
	releasePostRest := make(chan struct{})
	done := make(chan struct{})
	errCh := make(chan error, 1)

	service := newServicePlan(
		"api",
		fakeRolloutOperation{
			useKind: true,
			kind:    rolloutOperationRun,
			executeForRolloutFn: func(context.Context, operation.Client) operationExecution {
				close(canaryStarted)
				<-releaseCanary
				return operationExecution{}
			},
		},
		fakeRolloutOperation{
			useKind: true,
			kind:    rolloutOperationRun,
			executeForRolloutFn: func(context.Context, operation.Client) operationExecution {
				postStarted <- "one"
				<-releasePostOne
				return operationExecution{}
			},
		},
		fakeRolloutOperation{
			useKind: true,
			kind:    rolloutOperationRun,
			executeForRolloutFn: func(context.Context, operation.Client) operationExecution {
				postStarted <- "two"
				<-releasePostRest
				return operationExecution{}
			},
		},
		fakeRolloutOperation{
			useKind: true,
			kind:    rolloutOperationRun,
			executeForRolloutFn: func(context.Context, operation.Client) operationExecution {
				postStarted <- "three"
				<-releasePostRest
				return operationExecution{}
			},
		},
	)
	service.IsNewService = true
	service.Spec.UpdateConfig.Parallelism = api.AsPtr(uint64(2))

	plan := Plan{Services: []*deploy.ServicePlan{service}}
	go func() {
		defer close(done)
		_, err := plan.ExecuteRollout(context.Background(), nil, RolloutOptions{Parallel: true})
		errCh <- err
	}()

	<-canaryStarted
	close(releaseCanary)
	first := <-postStarted
	second := <-postStarted
	assert.ElementsMatch(t, []string{"one", "two"}, []string{first, second})
	select {
	case started := <-postStarted:
		t.Fatalf("unexpected early third rollout start: %s", started)
	default:
	}
	close(releasePostOne)
	assert.Equal(t, "three", <-postStarted)
	close(releasePostRest)
	<-done
	require.NoError(t, <-errCh)
}

func TestResolveRolloutParallelismAndStopFirstSerialisation(t *testing.T) {
	service := newServicePlan("db")
	service.Spec.UpdateConfig.Parallelism = api.AsPtr(uint64(10))

	assert.Equal(t, uint64(10), resolveRolloutParallelism(service))
	assert.False(t, isParallelizableRolloutOperation(fakeRolloutOperation{
		useKind: true,
		kind:    rolloutOperationReplaceStopFirst,
	}))
}

type fakeOperation struct {
	execute func(context.Context) error
}

func (o fakeOperation) Execute(ctx context.Context, _ operation.Client) error {
	if o.execute == nil {
		return nil
	}
	return o.execute(ctx)
}

func (o fakeOperation) Format() string {
	return "fake"
}

func (o fakeOperation) String() string {
	return "fakeOperation"
}

type fakeRolloutOperation struct {
	useKind             bool
	kind                rolloutOperationKind
	executeForRolloutFn func(context.Context, operation.Client) operationExecution
}

func (o fakeRolloutOperation) Execute(ctx context.Context, cli operation.Client) error {
	return o.executeForRollout(ctx, cli).err
}

func (o fakeRolloutOperation) Format() string {
	return "fakeRolloutOperation"
}

func (o fakeRolloutOperation) String() string {
	return "fakeRolloutOperation"
}

func (o fakeRolloutOperation) executeForRollout(ctx context.Context, cli operation.Client) operationExecution {
	if o.executeForRolloutFn == nil {
		return operationExecution{}
	}
	return o.executeForRolloutFn(ctx, cli)
}

func (o fakeRolloutOperation) rolloutOperationKind() rolloutOperationKind {
	if o.useKind {
		return o.kind
	}
	return rolloutOperationReplaceStartFirst
}

func newServicePlan(name string, ops ...operation.Operation) *deploy.ServicePlan {
	return &deploy.ServicePlan{
		ServiceName: name,
		SequenceOperation: operation.SequenceOperation{
			Operations: ops,
		},
	}
}

func batchNames(batch []*deploy.ServicePlan) []string {
	names := make([]string, 0, len(batch))
	for _, sp := range batch {
		names = append(names, sp.ServiceName)
	}
	return names
}

func indexOf(steps []string, want string) int {
	for i, step := range steps {
		if step == want {
			return i
		}
	}
	return -1
}
