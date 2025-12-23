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
	// ErrLockLost is returned when the lock was lost (e.g., expired and taken by another deployment).
	ErrLockLost = errors.New("lock was lost")
)

// DeploymentLock represents a distributed lock record for deployments.
type DeploymentLock struct {
	LockKey      string
	DeploymentID string
	Owner        string
	// Generation is a monotonically increasing fencing token. Each acquire increments this value.
	// Operations should validate that generation matches to prevent stale operations.
	Generation int64
	AcquiredAt time.Time
	ExpiresAt  time.Time
}

// AcquireLockResult contains the result of a lock acquisition attempt.
type AcquireLockResult struct {
	// Acquired indicates whether the lock was successfully acquired.
	Acquired bool
	// Lock contains the lock state (current holder if not acquired, or new lock if acquired).
	Lock *DeploymentLock
}

// AcquireLock attempts to acquire a deployment lock for the given key.
// If the lock is already held by another deployment (and not expired), returns Acquired=false with current lock info.
// If the lock exists but is expired, it will be replaced with the new lock (generation incremented).
// The ttl parameter specifies how long the lock should be held before it expires automatically.
//
// This is an atomic compare-and-swap operation using SQLite's INSERT OR REPLACE with proper conditions.
// The generation is incremented on each successful acquire to serve as a fencing token.
func (s *Store) AcquireLock(ctx context.Context, lockKey, deploymentID, owner string, ttl time.Duration) (*AcquireLockResult, error) {
	// Use server-side time consistently (datetime('now')) for all timestamp comparisons.
	// TTL is added as seconds to the server time to compute expires_at.
	ttlSeconds := int64(ttl.Seconds())

	// Atomic acquire using INSERT with ON CONFLICT.
	// The UPDATE only happens if:
	// 1. The lock has expired (expires_at < datetime('now')), OR
	// 2. The same deployment is re-acquiring (allows idempotent acquire)
	//
	// On successful acquire (insert or expired takeover), generation is incremented.
	// On re-acquire by same deployment, generation stays the same.
	res, err := s.corro.ExecContext(ctx, `
		INSERT INTO deployment_locks (lock_key, deployment_id, owner, generation, acquired_at, expires_at)
		VALUES (?, ?, ?, 1, datetime('now'), datetime('now', '+' || ? || ' seconds'))
		ON CONFLICT (lock_key) DO UPDATE SET
			deployment_id = excluded.deployment_id,
			owner = excluded.owner,
			generation = CASE
				WHEN deployment_locks.deployment_id = excluded.deployment_id THEN deployment_locks.generation
				ELSE deployment_locks.generation + 1
			END,
			acquired_at = datetime('now'),
			expires_at = datetime('now', '+' || ? || ' seconds')
		WHERE deployment_locks.expires_at < datetime('now')
		   OR deployment_locks.deployment_id = excluded.deployment_id`,
		lockKey, deploymentID, owner, ttlSeconds, ttlSeconds)
	if err != nil {
		return nil, fmt.Errorf("acquire lock query: %w", err)
	}

	// Fetch the current lock state regardless of outcome.
	lock, err := s.GetLock(ctx, lockKey)
	if err != nil {
		return nil, fmt.Errorf("get lock after acquire: %w", err)
	}

	if res.RowsAffected == 0 {
		// Lock is held by another deployment and not expired.
		slog.Debug("Deployment lock held by another.",
			"lock_key", lockKey,
			"holder", lock.DeploymentID,
			"owner", lock.Owner,
			"expires", lock.ExpiresAt)
		return &AcquireLockResult{
			Acquired: false,
			Lock:     lock,
		}, nil
	}

	slog.Debug("Deployment lock acquired.",
		"lock_key", lockKey,
		"deployment_id", deploymentID,
		"owner", owner,
		"generation", lock.Generation)

	return &AcquireLockResult{
		Acquired: true,
		Lock:     lock,
	}, nil
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
// The generation must match the expected value to prevent stale refresh.
// Returns ErrLockNotHeld if the lock is not held by this deployment.
// Returns ErrLockLost if the generation doesn't match (lock was taken over).
func (s *Store) RefreshLock(ctx context.Context, lockKey, deploymentID string, generation int64, ttl time.Duration) error {
	ttlSeconds := int64(ttl.Seconds())

	// Update only if deployment_id AND generation match (fencing).
	// Use server-side time for consistency.
	res, err := s.corro.ExecContext(ctx, `
		UPDATE deployment_locks
		SET expires_at = datetime('now', '+' || ? || ' seconds')
		WHERE lock_key = ? AND deployment_id = ? AND generation = ?`,
		ttlSeconds, lockKey, deploymentID, generation)
	if err != nil {
		return fmt.Errorf("refresh lock query: %w", err)
	}

	if res.RowsAffected == 0 {
		// Check why it failed: lock doesn't exist, wrong owner, or wrong generation?
		lock, err := s.GetLock(ctx, lockKey)
		if err != nil {
			return fmt.Errorf("check lock after failed refresh: %w", err)
		}
		if lock == nil {
			return ErrLockNotHeld
		}
		if lock.DeploymentID != deploymentID {
			return fmt.Errorf("%w: now held by %s", ErrLockLost, lock.DeploymentID)
		}
		if lock.Generation != generation {
			return fmt.Errorf("%w: generation mismatch (expected %d, got %d)", ErrLockLost, generation, lock.Generation)
		}
		// Shouldn't happen, but fall back to generic error
		return ErrLockNotHeld
	}

	slog.Debug("Deployment lock refreshed.",
		"lock_key", lockKey,
		"deployment_id", deploymentID,
		"generation", generation)
	return nil
}

// ValidateLock checks if the lock is still held by this deployment with the expected generation.
// This can be called before critical operations to ensure the lock hasn't been lost.
func (s *Store) ValidateLock(ctx context.Context, lockKey, deploymentID string, generation int64) error {
	lock, err := s.GetLock(ctx, lockKey)
	if err != nil {
		return fmt.Errorf("get lock: %w", err)
	}
	if lock == nil {
		return ErrLockNotHeld
	}
	if lock.DeploymentID != deploymentID {
		return fmt.Errorf("%w: now held by %s", ErrLockLost, lock.DeploymentID)
	}
	if lock.Generation != generation {
		return fmt.Errorf("%w: generation mismatch (expected %d, got %d)", ErrLockLost, generation, lock.Generation)
	}
	return nil
}

// GetLock returns the current lock for the given key, or nil if no lock exists.
func (s *Store) GetLock(ctx context.Context, lockKey string) (*DeploymentLock, error) {
	rows, err := s.corro.QueryContext(ctx, `
		SELECT lock_key, deployment_id, owner, generation, acquired_at, expires_at
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
	if err = rows.Scan(&lock.LockKey, &lock.DeploymentID, &lock.Owner, &lock.Generation, &acquiredAtStr, &expiresAtStr); err != nil {
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
// This is optional maintenance - correctness does not depend on it.
// The acquire logic handles expired locks automatically.
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
