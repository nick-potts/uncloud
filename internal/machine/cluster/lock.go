//go:build ignore
// +build ignore

// Package cluster implements cluster-level operations including deployment locking.
//
// NOTE: This file is excluded from build until the protobuf definitions are regenerated.
// To enable this file:
// 1. Run `make proto` to generate the required types
// 2. Remove the "//go:build ignore" line at the top of this file
package cluster

import (
	"context"
	"errors"
	"time"

	"github.com/psviderski/uncloud/internal/machine/api/pb"
	"github.com/psviderski/uncloud/internal/machine/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// AcquireDeploymentLock attempts to acquire a deployment lock for the given key.
// This method will be available via gRPC once the protobuf files are regenerated with `make proto`.
func (c *Cluster) AcquireDeploymentLock(
	ctx context.Context, req *pb.AcquireDeploymentLockRequest,
) (*pb.AcquireDeploymentLockResponse, error) {
	if err := c.checkInitialised(ctx); err != nil {
		return nil, err
	}

	if req.LockKey == "" {
		return nil, status.Error(codes.InvalidArgument, "lock_key not set")
	}
	if req.DeploymentId == "" {
		return nil, status.Error(codes.InvalidArgument, "deployment_id not set")
	}
	if req.TtlSeconds <= 0 {
		return nil, status.Error(codes.InvalidArgument, "ttl_seconds must be positive")
	}

	ttl := time.Duration(req.TtlSeconds) * time.Second
	err := c.store.AcquireLock(ctx, req.LockKey, req.DeploymentId, req.Owner, ttl)
	if err != nil {
		if errors.Is(err, store.ErrLockHeld) {
			// Lock is held by another deployment - return the current lock info.
			lock, getErr := c.store.GetLock(ctx, req.LockKey)
			if getErr != nil {
				return nil, status.Errorf(codes.Internal, "get existing lock: %v", getErr)
			}
			return &pb.AcquireDeploymentLockResponse{
				Acquired: false,
				Lock:     storeLockToPb(lock),
			}, nil
		}
		return nil, status.Errorf(codes.Internal, "acquire lock: %v", err)
	}

	// Lock acquired successfully - return the new lock info.
	lock, err := c.store.GetLock(ctx, req.LockKey)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get acquired lock: %v", err)
	}

	return &pb.AcquireDeploymentLockResponse{
		Acquired: true,
		Lock:     storeLockToPb(lock),
	}, nil
}

// ReleaseDeploymentLock releases a deployment lock if it is held by the given deployment.
func (c *Cluster) ReleaseDeploymentLock(
	ctx context.Context, req *pb.ReleaseDeploymentLockRequest,
) (*emptypb.Empty, error) {
	if err := c.checkInitialised(ctx); err != nil {
		return nil, err
	}

	if req.LockKey == "" {
		return nil, status.Error(codes.InvalidArgument, "lock_key not set")
	}
	if req.DeploymentId == "" {
		return nil, status.Error(codes.InvalidArgument, "deployment_id not set")
	}

	err := c.store.ReleaseLock(ctx, req.LockKey, req.DeploymentId)
	if err != nil {
		if errors.Is(err, store.ErrLockNotHeld) {
			return nil, status.Error(codes.FailedPrecondition, "lock not held by this deployment")
		}
		return nil, status.Errorf(codes.Internal, "release lock: %v", err)
	}

	return &emptypb.Empty{}, nil
}

// RefreshDeploymentLock extends the TTL of a lock held by the given deployment.
func (c *Cluster) RefreshDeploymentLock(
	ctx context.Context, req *pb.RefreshDeploymentLockRequest,
) (*emptypb.Empty, error) {
	if err := c.checkInitialised(ctx); err != nil {
		return nil, err
	}

	if req.LockKey == "" {
		return nil, status.Error(codes.InvalidArgument, "lock_key not set")
	}
	if req.DeploymentId == "" {
		return nil, status.Error(codes.InvalidArgument, "deployment_id not set")
	}
	if req.TtlSeconds <= 0 {
		return nil, status.Error(codes.InvalidArgument, "ttl_seconds must be positive")
	}

	ttl := time.Duration(req.TtlSeconds) * time.Second
	err := c.store.RefreshLock(ctx, req.LockKey, req.DeploymentId, ttl)
	if err != nil {
		if errors.Is(err, store.ErrLockNotHeld) {
			return nil, status.Error(codes.FailedPrecondition, "lock not held by this deployment")
		}
		return nil, status.Errorf(codes.Internal, "refresh lock: %v", err)
	}

	return &emptypb.Empty{}, nil
}

// GetDeploymentLock returns the current lock for the given key.
func (c *Cluster) GetDeploymentLock(
	ctx context.Context, req *pb.GetDeploymentLockRequest,
) (*pb.DeploymentLock, error) {
	if err := c.checkInitialised(ctx); err != nil {
		return nil, err
	}

	if req.LockKey == "" {
		return nil, status.Error(codes.InvalidArgument, "lock_key not set")
	}

	lock, err := c.store.GetLock(ctx, req.LockKey)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get lock: %v", err)
	}
	if lock == nil {
		return nil, status.Error(codes.NotFound, "lock not found")
	}

	return storeLockToPb(lock), nil
}

// storeLockToPb converts a store.DeploymentLock to a pb.DeploymentLock.
func storeLockToPb(lock *store.DeploymentLock) *pb.DeploymentLock {
	if lock == nil {
		return nil
	}
	return &pb.DeploymentLock{
		LockKey:      lock.LockKey,
		DeploymentId: lock.DeploymentID,
		Owner:        lock.Owner,
		AcquiredAt:   lock.AcquiredAt.Format(time.RFC3339),
		ExpiresAt:    lock.ExpiresAt.Format(time.RFC3339),
	}
}
