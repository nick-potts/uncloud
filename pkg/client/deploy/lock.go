package deploy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	// DefaultLockTTL is the default time-to-live for deployment locks.
	DefaultLockTTL = 5 * time.Minute

	// LockRefreshInterval is how often the lock should be refreshed during long deployments.
	// Should be less than TTL/2 to ensure we refresh before expiry.
	LockRefreshInterval = 90 * time.Second

	// ServiceLockKeyPrefix is the prefix for service-level locks.
	ServiceLockKeyPrefix = "service:"

	// ProjectLockKeyPrefix is the prefix for project-level locks.
	ProjectLockKeyPrefix = "project:"
)

var (
	// ErrDeploymentLocked is returned when a deployment lock cannot be acquired.
	ErrDeploymentLocked = errors.New("deployment is locked by another operation")

	// ErrLockNotAvailable is returned when distributed locking is not available and strict mode is enabled.
	ErrLockNotAvailable = errors.New("distributed lock not available")

	// ErrLockLost is returned when the lock was lost during execution.
	ErrLockLost = errors.New("deployment lock was lost")
)

// LockClient defines the interface for deployment lock operations.
// This interface is implemented by the gRPC client once protobuf files are regenerated.
type LockClient interface {
	// AcquireDeploymentLock attempts to acquire a lock.
	// Returns (acquired, generation, error). If acquired is false, the lock is held by another.
	AcquireDeploymentLock(ctx context.Context, lockKey, deploymentID, owner string, ttl time.Duration) (acquired bool, generation int64, holderInfo string, err error)

	// ReleaseDeploymentLock releases a lock held by this deployment.
	ReleaseDeploymentLock(ctx context.Context, lockKey, deploymentID string) error

	// RefreshDeploymentLock extends the TTL. Generation must match.
	RefreshDeploymentLock(ctx context.Context, lockKey, deploymentID string, generation int64, ttl time.Duration) error

	// ValidateDeploymentLock checks if the lock is still held with expected generation.
	ValidateDeploymentLock(ctx context.Context, lockKey, deploymentID string, generation int64) error
}

// LockMode controls how the deployment lock behaves.
type LockMode int

const (
	// LockModeStrict requires distributed locking. Fails if lock client is unavailable.
	LockModeStrict LockMode = iota

	// LockModeDisabled skips locking entirely. Use only for emergencies.
	LockModeDisabled
)

// DeploymentLock manages a distributed lock for deployment operations.
// It ensures that concurrent deployments of the same service or project don't conflict.
type DeploymentLock struct {
	client       LockClient
	lockKey      string
	deploymentID string
	owner        string
	ttl          time.Duration
	mode         LockMode

	mu         sync.Mutex
	acquired   bool
	generation int64
	lost       bool // Set to true if we detected lock loss
	cancelCtx  context.CancelFunc
}

// NewDeploymentLock creates a new deployment lock for the given key.
// The deploymentID is automatically generated.
// By default, uses LockModeStrict which requires a lock client.
func NewDeploymentLock(client LockClient, lockKey, owner string) *DeploymentLock {
	return &DeploymentLock{
		client:       client,
		lockKey:      lockKey,
		deploymentID: uuid.New().String(),
		owner:        owner,
		ttl:          DefaultLockTTL,
		mode:         LockModeStrict,
	}
}

// NewDisabledLock creates a lock that does nothing (for --no-lock mode).
func NewDisabledLock(lockKey string) *DeploymentLock {
	return &DeploymentLock{
		lockKey:      lockKey,
		deploymentID: uuid.New().String(),
		mode:         LockModeDisabled,
	}
}

// ServiceLockKey returns the lock key for a service.
func ServiceLockKey(serviceName string) string {
	return ServiceLockKeyPrefix + serviceName
}

// ProjectLockKey returns the lock key for a compose project.
func ProjectLockKey(projectName string) string {
	return ProjectLockKeyPrefix + projectName
}

// LockKey returns the lock key for this lock.
func (l *DeploymentLock) LockKey() string {
	return l.lockKey
}

// Acquire attempts to acquire the deployment lock.
// Returns ErrLockNotAvailable if no lock client is available (strict mode).
// Returns ErrDeploymentLocked if the lock is held by another deployment.
func (l *DeploymentLock) Acquire(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.acquired {
		return nil // Already acquired.
	}

	if l.mode == LockModeDisabled {
		slog.Warn("Deployment lock DISABLED. Concurrent deployments may conflict.",
			"lock_key", l.lockKey)
		l.acquired = true
		return nil
	}

	if l.client == nil {
		return fmt.Errorf("%w: run 'make proto' to enable distributed locking, or use --no-lock for emergencies",
			ErrLockNotAvailable)
	}

	acquired, generation, holderInfo, err := l.client.AcquireDeploymentLock(ctx, l.lockKey, l.deploymentID, l.owner, l.ttl)
	if err != nil {
		return fmt.Errorf("acquire deployment lock: %w", err)
	}
	if !acquired {
		return fmt.Errorf("%w: %s (lock key: %s)", ErrDeploymentLocked, holderInfo, l.lockKey)
	}

	l.acquired = true
	l.generation = generation
	l.lost = false

	// Start background refresh goroutine.
	refreshCtx, cancel := context.WithCancel(context.Background())
	l.cancelCtx = cancel
	go l.refreshLoop(refreshCtx)

	slog.Debug("Deployment lock acquired.",
		"lock_key", l.lockKey,
		"deployment_id", l.deploymentID,
		"generation", l.generation)
	return nil
}

// Release releases the deployment lock.
func (l *DeploymentLock) Release(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.acquired {
		return nil // Not acquired, nothing to release.
	}

	// Stop the refresh goroutine.
	if l.cancelCtx != nil {
		l.cancelCtx()
		l.cancelCtx = nil
	}

	l.acquired = false

	if l.mode == LockModeDisabled || l.client == nil {
		return nil // No distributed lock to release.
	}

	err := l.client.ReleaseDeploymentLock(ctx, l.lockKey, l.deploymentID)
	if err != nil {
		// Log but don't return error - lock will expire anyway.
		slog.Warn("Failed to release deployment lock (will expire automatically).",
			"lock_key", l.lockKey, "deployment_id", l.deploymentID, "error", err)
		return nil
	}

	slog.Debug("Deployment lock released.",
		"lock_key", l.lockKey,
		"deployment_id", l.deploymentID)
	return nil
}

// refreshLoop periodically refreshes the lock to prevent expiration during long deployments.
// If refresh fails (lock lost), it marks the lock as lost and stops.
func (l *DeploymentLock) refreshLoop(ctx context.Context) {
	// Add jitter (±10%) to prevent thundering herd if multiple locks exist.
	jitterRange := float64(LockRefreshInterval) * 0.1
	nextInterval := func() time.Duration {
		jitter := time.Duration(rand.Float64()*2*jitterRange - jitterRange)
		return LockRefreshInterval + jitter
	}

	timer := time.NewTimer(nextInterval())
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			l.mu.Lock()
			if !l.acquired || l.client == nil || l.mode == LockModeDisabled {
				l.mu.Unlock()
				return
			}

			generation := l.generation
			l.mu.Unlock()

			err := l.client.RefreshDeploymentLock(context.Background(), l.lockKey, l.deploymentID, generation, l.ttl)

			l.mu.Lock()
			if err != nil {
				slog.Error("LOCK LOST: Failed to refresh deployment lock. Deployment should halt.",
					"lock_key", l.lockKey,
					"deployment_id", l.deploymentID,
					"generation", generation,
					"error", err)
				l.lost = true
				l.mu.Unlock()
				return // Stop refreshing - lock is lost
			}

			slog.Debug("Deployment lock refreshed.",
				"lock_key", l.lockKey,
				"deployment_id", l.deploymentID,
				"generation", generation)
			l.mu.Unlock()

			// Reset timer with new jittered interval for next refresh.
			timer.Reset(nextInterval())
		}
	}
}

// Validate checks if the lock is still held. Should be called before critical operations.
// Returns ErrLockLost if the lock was lost.
func (l *DeploymentLock) Validate(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.mode == LockModeDisabled {
		return nil
	}

	if !l.acquired {
		return fmt.Errorf("%w: lock was never acquired", ErrLockLost)
	}

	if l.lost {
		return fmt.Errorf("%w: detected during refresh", ErrLockLost)
	}

	if l.client == nil {
		return nil // Can't validate without client
	}

	// Active check against the store
	err := l.client.ValidateDeploymentLock(ctx, l.lockKey, l.deploymentID, l.generation)
	if err != nil {
		l.lost = true
		return fmt.Errorf("%w: %v", ErrLockLost, err)
	}

	return nil
}

// IsAcquired returns whether the lock is currently held (not considering if lost).
func (l *DeploymentLock) IsAcquired() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.acquired
}

// IsLost returns true if the lock was detected as lost.
func (l *DeploymentLock) IsLost() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lost
}

// LockKey returns the lock key.
func (l *DeploymentLock) LockKey() string {
	return l.lockKey
}

// DeploymentID returns the deployment ID.
func (l *DeploymentLock) DeploymentID() string {
	return l.deploymentID
}

// Generation returns the lock generation (fencing token).
func (l *DeploymentLock) Generation() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.generation
}

// WithLock executes the given function while holding the deployment lock.
// The lock is automatically released when the function returns (success or failure).
func WithLock(ctx context.Context, lock *DeploymentLock, fn func() error) error {
	if err := lock.Acquire(ctx); err != nil {
		return err
	}
	defer func() {
		if releaseErr := lock.Release(ctx); releaseErr != nil {
			slog.Warn("Failed to release deployment lock.", "error", releaseErr)
		}
	}()

	return fn()
}
