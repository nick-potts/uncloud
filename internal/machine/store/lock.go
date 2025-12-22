package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

var (
	// ErrLockHeld is returned when attempting to acquire a lock that is already held by another deployment.
	ErrLockHeld = errors.New("lock is held by another deployment")
	// ErrLockNotHeld is returned when attempting to release or refresh a lock that is not held by this deployment.
	ErrLockNotHeld = errors.New("lock is not held by this deployment")
)

// DeploymentLock represents a distributed lock record for deployments.
type DeploymentLock struct {
	LockKey      string
	DeploymentID string
	Owner        string
	AcquiredAt   time.Time
	ExpiresAt    time.Time
}

// AcquireLock attempts to acquire a deployment lock for the given key.
// If the lock is already held by another deployment (and not expired), ErrLockHeld is returned.
// If the lock exists but is expired, it will be replaced with the new lock.
// The ttl parameter specifies how long the lock should be held before it expires automatically.
func (s *Store) AcquireLock(ctx context.Context, lockKey, deploymentID, owner string, ttl time.Duration) error {
	expiresAt := time.Now().Add(ttl)

	// Try to insert a new lock, or replace an expired lock.
	// The INSERT will succeed if:
	// 1. No lock exists for this key, OR
	// 2. The existing lock has expired (expires_at < now), OR
	// 3. The existing lock is held by the same deployment (allows re-acquisition)
	res, err := s.corro.ExecContext(ctx, `
		INSERT INTO deployment_locks (lock_key, deployment_id, owner, acquired_at, expires_at)
		VALUES (?, ?, ?, datetime('now'), ?)
		ON CONFLICT (lock_key) DO UPDATE SET
			deployment_id = excluded.deployment_id,
			owner = excluded.owner,
			acquired_at = excluded.acquired_at,
			expires_at = excluded.expires_at
		WHERE deployment_locks.expires_at < datetime('now')
		   OR deployment_locks.deployment_id = excluded.deployment_id`,
		lockKey, deploymentID, owner, expiresAt.UTC().Format(time.DateTime))
	if err != nil {
		return fmt.Errorf("acquire lock query: %w", err)
	}

	if res.RowsAffected == 0 {
		// Lock is held by another deployment and not expired.
		// Fetch the current lock holder for better error reporting.
		lock, err := s.GetLock(ctx, lockKey)
		if err != nil {
			return fmt.Errorf("%w (failed to get lock holder: %v)", ErrLockHeld, err)
		}
		return fmt.Errorf("%w: held by deployment %s (owner: %s, expires: %s)",
			ErrLockHeld, lock.DeploymentID, lock.Owner, lock.ExpiresAt.Format(time.RFC3339))
	}

	slog.Debug("Deployment lock acquired.", "lock_key", lockKey, "deployment_id", deploymentID, "owner", owner)
	return nil
}

// ReleaseLock releases a deployment lock if it is held by the given deployment.
// Returns ErrLockNotHeld if the lock is not held by this deployment.
func (s *Store) ReleaseLock(ctx context.Context, lockKey, deploymentID string) error {
	res, err := s.corro.ExecContext(ctx, `
		DELETE FROM deployment_locks
		WHERE lock_key = ? AND deployment_id = ?`,
		lockKey, deploymentID)
	if err != nil {
		return fmt.Errorf("release lock query: %w", err)
	}

	if res.RowsAffected == 0 {
		return ErrLockNotHeld
	}

	slog.Debug("Deployment lock released.", "lock_key", lockKey, "deployment_id", deploymentID)
	return nil
}

// RefreshLock extends the TTL of a lock held by the given deployment.
// Returns ErrLockNotHeld if the lock is not held by this deployment.
func (s *Store) RefreshLock(ctx context.Context, lockKey, deploymentID string, ttl time.Duration) error {
	expiresAt := time.Now().Add(ttl)

	res, err := s.corro.ExecContext(ctx, `
		UPDATE deployment_locks
		SET expires_at = ?
		WHERE lock_key = ? AND deployment_id = ?`,
		expiresAt.UTC().Format(time.DateTime), lockKey, deploymentID)
	if err != nil {
		return fmt.Errorf("refresh lock query: %w", err)
	}

	if res.RowsAffected == 0 {
		return ErrLockNotHeld
	}

	slog.Debug("Deployment lock refreshed.", "lock_key", lockKey, "deployment_id", deploymentID)
	return nil
}

// GetLock returns the current lock for the given key, or nil if no lock exists.
func (s *Store) GetLock(ctx context.Context, lockKey string) (*DeploymentLock, error) {
	rows, err := s.corro.QueryContext(ctx, `
		SELECT lock_key, deployment_id, owner, acquired_at, expires_at
		FROM deployment_locks
		WHERE lock_key = ?`,
		lockKey)
	if err != nil {
		return nil, fmt.Errorf("get lock query: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if rows.Err() != nil {
			return nil, rows.Err()
		}
		return nil, nil
	}

	var lock DeploymentLock
	var acquiredAtStr, expiresAtStr string
	if err = rows.Scan(&lock.LockKey, &lock.DeploymentID, &lock.Owner, &acquiredAtStr, &expiresAtStr); err != nil {
		return nil, fmt.Errorf("scan lock: %w", err)
	}

	if lock.AcquiredAt, err = time.Parse(time.DateTime, acquiredAtStr); err != nil {
		return nil, fmt.Errorf("parse acquired_at: %w", err)
	}
	if lock.ExpiresAt, err = time.Parse(time.DateTime, expiresAtStr); err != nil {
		return nil, fmt.Errorf("parse expires_at: %w", err)
	}

	return &lock, nil
}

// CleanupExpiredLocks removes all expired locks from the database.
// This is useful for periodic cleanup, though locks are also cleaned up opportunistically
// when new locks are acquired.
func (s *Store) CleanupExpiredLocks(ctx context.Context) (int64, error) {
	res, err := s.corro.ExecContext(ctx, `
		DELETE FROM deployment_locks
		WHERE expires_at < datetime('now')`)
	if err != nil {
		return 0, fmt.Errorf("cleanup expired locks query: %w", err)
	}

	if res.RowsAffected > 0 {
		slog.Debug("Expired deployment locks cleaned up.", "count", res.RowsAffected)
	}
	return res.RowsAffected, nil
}
