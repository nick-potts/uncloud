package deploy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	// DefaultLockTTL is the default time-to-live for deployment locks.
	DefaultLockTTL = 5 * time.Minute

	// LockRefreshInterval is how often the lock should be refreshed during long deployments.
	LockRefreshInterval = 1 * time.Minute

	// ServiceLockKeyPrefix is the prefix for service-level locks.
	ServiceLockKeyPrefix = "service:"

	// ProjectLockKeyPrefix is the prefix for project-level locks.
	ProjectLockKeyPrefix = "project:"
)

var (
	// ErrDeploymentLocked is returned when a deployment lock cannot be acquired.
	ErrDeploymentLocked = errors.New("deployment is locked by another operation")
)

// LockClient defines the interface for deployment lock operations.
// This interface is implemented by the gRPC client once protobuf files are regenerated.
type LockClient interface {
	AcquireDeploymentLock(ctx context.Context, lockKey, deploymentID, owner string, ttl time.Duration) (bool, error)
	ReleaseDeploymentLock(ctx context.Context, lockKey, deploymentID string) error
	RefreshDeploymentLock(ctx context.Context, lockKey, deploymentID string, ttl time.Duration) error
}

// DeploymentLock manages a distributed lock for deployment operations.
// It ensures that concurrent deployments of the same service or project don't conflict.
type DeploymentLock struct {
	client       LockClient
	lockKey      string
	deploymentID string
	owner        string
	ttl          time.Duration

	mu        sync.Mutex
	acquired  bool
	cancelCtx context.CancelFunc
}

// NewDeploymentLock creates a new deployment lock for the given key.
// The deploymentID is automatically generated if empty.
func NewDeploymentLock(client LockClient, lockKey, owner string) *DeploymentLock {
	return &DeploymentLock{
		client:       client,
		lockKey:      lockKey,
		deploymentID: uuid.New().String(),
		owner:        owner,
		ttl:          DefaultLockTTL,
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

// Acquire attempts to acquire the deployment lock.
// If the lock is already held by another deployment, ErrDeploymentLocked is returned.
func (l *DeploymentLock) Acquire(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.acquired {
		return nil // Already acquired.
	}

	if l.client == nil {
		// No lock client available - log warning and continue without distributed locking.
		slog.Warn("Distributed deployment lock not available. Run 'make proto' to enable.",
			"lock_key", l.lockKey, "deployment_id", l.deploymentID)
		l.acquired = true
		return nil
	}

	acquired, err := l.client.AcquireDeploymentLock(ctx, l.lockKey, l.deploymentID, l.owner, l.ttl)
	if err != nil {
		return fmt.Errorf("acquire deployment lock: %w", err)
	}
	if !acquired {
		return fmt.Errorf("%w: lock key %s is held by another deployment", ErrDeploymentLocked, l.lockKey)
	}

	l.acquired = true

	// Start background refresh goroutine.
	refreshCtx, cancel := context.WithCancel(context.Background())
	l.cancelCtx = cancel
	go l.refreshLoop(refreshCtx)

	slog.Debug("Deployment lock acquired.", "lock_key", l.lockKey, "deployment_id", l.deploymentID)
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

	if l.client == nil {
		return nil // No distributed lock to release.
	}

	err := l.client.ReleaseDeploymentLock(ctx, l.lockKey, l.deploymentID)
	if err != nil {
		// Log but don't return error - lock will expire anyway.
		slog.Warn("Failed to release deployment lock (will expire automatically).",
			"lock_key", l.lockKey, "deployment_id", l.deploymentID, "error", err)
		return nil
	}

	slog.Debug("Deployment lock released.", "lock_key", l.lockKey, "deployment_id", l.deploymentID)
	return nil
}

// refreshLoop periodically refreshes the lock to prevent expiration during long deployments.
func (l *DeploymentLock) refreshLoop(ctx context.Context) {
	ticker := time.NewTicker(LockRefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.mu.Lock()
			if !l.acquired || l.client == nil {
				l.mu.Unlock()
				return
			}

			err := l.client.RefreshDeploymentLock(context.Background(), l.lockKey, l.deploymentID, l.ttl)
			if err != nil {
				slog.Warn("Failed to refresh deployment lock.",
					"lock_key", l.lockKey, "deployment_id", l.deploymentID, "error", err)
			} else {
				slog.Debug("Deployment lock refreshed.",
					"lock_key", l.lockKey, "deployment_id", l.deploymentID)
			}
			l.mu.Unlock()
		}
	}
}

// IsAcquired returns whether the lock is currently held.
func (l *DeploymentLock) IsAcquired() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.acquired
}

// LockKey returns the lock key.
func (l *DeploymentLock) LockKey() string {
	return l.lockKey
}

// DeploymentID returns the deployment ID.
func (l *DeploymentLock) DeploymentID() string {
	return l.deploymentID
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
