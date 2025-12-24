# Assessment: Is NATS Necessary for Uncloud Deploy Coordination?

## Executive Summary

**NATS is overkill for Uncloud's deploy coordination needs.** The current codebase has identified gaps in deploy concurrency control, but these can be addressed with simpler mechanisms that align with Uncloud's AP-leaning, decentralized architecture. A CP-style lease/fencing mechanism (like NATS JetStream KV) would contradict Uncloud's core design philosophy of operating without a central control plane.

---

## Analysis Findings

### 1. Deploy Lifecycle Architecture

The deploy lifecycle is cleanly separated into two phases:

**Planning Phase** (`pkg/client/deploy/deploy.go:65-102`):
- Captures cluster state via `scheduler.InspectClusterState()`
- Evaluates constraints and eligible machines
- Generates a sequence of operations
- **Read-only** - no mutations occur

**Execution Phase** (`pkg/client/deploy/operation.go:166-172`):
- Executes operations sequentially via `SequenceOperation.Execute()`
- Calls `CreateContainer()`, `StartContainer()`, `RemoveContainer()`
- **Mutations occur** - but no re-validation of state

**Key observation**: Machines are NOT created during deploys. They must exist beforehand via `uc machine init/add`. The "duplicate machines" concern doesn't apply to deploys - it's a separate machine management issue.

### 2. Exact Code Paths Leading to Duplicate Containers

**Race Window #1: Concurrent Plan→Execute**
```
Deploy A: Plan() at T=0  →  state shows 0 replicas needed
Deploy B: Plan() at T=1  →  state shows 0 replicas needed (same snapshot)
         ↓ (both plans include RunContainerOperation for same service)
Deploy A: Execute() at T=2  →  creates container with random suffix "-ab12"
Deploy B: Execute() at T=3  →  creates container with different suffix "-cd34"
Result: 2 containers exist when only 1 was desired
```

The race exists because:
1. `InspectClusterState()` captures a point-in-time snapshot (`deploy.go:90`)
2. State is NOT re-read before execution (`deploy.go:151`)
3. Container names have random suffixes (`container.go:41`: `RandomAlphaNumeric(4)`)
4. No uniqueness constraint on `(service_id, machine_id)` in schema (`schema.sql:21`)

**Race Window #2: Partition Scenario**
```
Machine 1 (partition A): uc deploy service-x → creates containers
Machine 2 (partition B): uc deploy service-x → creates containers
Partition heals → both sets of containers exist
Corrosion CRDT merges → duplicate containers in store
```

### 3. Missing Invariant

The missing invariant is: **"At most one deploy execution for a given service should proceed at any moment."**

This is explicitly acknowledged in the code:
```go
// TODO: forbid to run the same deployment more than once.
// pkg/client/deploy/deploy.go:144
```

### 4. What Primitives Could Enforce This Invariant?

| Approach | Where It Integrates | Complexity | Consistency Model |
|----------|---------------------|------------|-------------------|
| **Per-service mutex in memory** | `Deployment.Run()` | Low | Single-node only |
| **Corrosion-based lock row** | Before `Plan()` | Medium | Eventually consistent (problematic) |
| **Optimistic concurrency (generation/epoch)** | `InspectClusterState()` + `CreateContainer()` | Medium | Eventually consistent + retry |
| **NATS JetStream KV lease** | Before `Execute()` | High | Strongly consistent (CP) |
| **Idempotency keys on operations** | `RunContainerOperation.Execute()` | Medium | Eventually consistent + dedup |

### 5. Can This Be Solved Without CP Coordination?

**Yes.** The key insight is that Uncloud already tolerates eventual consistency for container state. The real problem isn't concurrent writes—it's the lack of **idempotent, deterministic container creation**.

**Root causes that don't require CP coordination:**
1. Random container suffixes (`RandomAlphaNumeric(4)`) prevent idempotency
2. No operation IDs or action tracking
3. No deduplication before container creation

**Solution approach without NATS:**

1. **Deterministic container naming** - Generate container names from `hash(service_id, spec_hash, replica_slot)` instead of random suffix
2. **Idempotent creation** - Check if container with target name exists before creating (Docker API already returns error on duplicate names - handle it gracefully)
3. **Epoch/generation on service spec** - Include a spec generation counter that increments on each deploy
4. **CRDT-safe deduplication** - In Corrosion schema, add `UNIQUE(service_id, machine_id, replica_slot)` constraint

This approach:
- Works during partitions (both partitions may create, but with same names → one wins)
- Converges correctly when partitions heal
- Doesn't require coordination before execution

### 6. Where NATS/JetStream Would Help vs. Hurt

**Would help:**
- Guaranteed single-execution of a deploy across entire cluster
- Clean lease semantics with automatic expiry
- Request-reply patterns for explicit coordination

**Would hurt:**
- Introduces a CP dependency into an AP system
- Single point of failure if NATS cluster is unavailable
- Contradicts design.md philosophy: "users should be able to add machines as compute resources without worrying about control plane high availability"
- Partition behavior becomes complex (which partition gets the lock?)
- Requires running and maintaining NATS cluster

### 7. The Design Document's Own Answer

From `misc/design.md:117-118`:
> "Not sure if we really need this yet, but more complex scenarios like running and watching a replica set of containers might require a sort of coordination between machines. I believe this can be achieved by using consensus and leader election algorithms **on demand**."

The key phrase is "on demand" - not as a permanent infrastructure dependency.

---

## Answers to Specific Questions

### What exact code paths lead to duplicated machines today?

**Machines are not duplicated during deploys.** Machine creation happens separately via `uc machine init/add`. Container duplication occurs due to:
1. Concurrent `Deployment.Run()` calls with stale state (`deploy.go:145-151`)
2. Random container suffixes preventing idempotent retries (`container.go:41`)
3. No uniqueness constraints in schema beyond Docker container ID

### What invariant is currently missing?

"A service's container count on a machine should not exceed the planned replica slots."
More specifically: "Container creation for a given `(service_id, machine_id, replica_slot)` tuple should be idempotent."

### What is the minimum additional primitive required?

**Deterministic, idempotent container naming + pre-creation existence check.**

This can be implemented entirely locally by:
1. Generating container name as `f(service_id, spec_hash, machine_id, slot_index)`
2. Before calling Docker `ContainerCreate`, check if container with that name exists
3. If exists and matches spec, treat as success (idempotent)
4. If exists and differs, follow update/recreate logic

### Can this be implemented entirely locally?

**Yes.** The fix is primarily in:
- `pkg/client/container.go` - deterministic naming
- `pkg/client/deploy/operation.go` - idempotent execution with existence checks
- `internal/machine/docker/server.go` - handle duplicate name gracefully

### Can this use existing Corrosion/CRDT mechanisms?

**Partially.** Corrosion can help with:
- Adding `UNIQUE(service_id, machine_id, slot_index)` constraint in `schema.sql`
- Storing intended container names before creation
- Tracking deploy epochs/generations

But the primary fix is at the Docker API call level, not the distributed store.

### Does it require a CP-style lease/fencing mechanism?

**No.** An AP-compatible idempotency approach is sufficient:
1. Deterministic naming ensures same container name across concurrent attempts
2. Docker's own atomic container creation prevents true duplicates
3. CRDT convergence handles divergent state after partition heals

---

## Recommendation

### **No NATS.**

Implement deploy coordination using:

1. **Deterministic container naming** (required)
   - File: `pkg/client/container.go:37-41`
   - Change: Replace `RandomAlphaNumeric(4)` with `hash(service_id, spec_hash, slot_index)`

2. **Idempotent operation execution** (required)
   - File: `pkg/client/deploy/operation.go:41-53`
   - Change: Before `CreateContainer`, check existence; if exists with matching spec, skip creation

3. **Schema uniqueness constraint** (recommended)
   - File: `internal/machine/store/schema.sql`
   - Add: `UNIQUE(service_id, machine_id)` or similar for global mode services

4. **Deploy generation counter** (optional, for observability)
   - Track deploy attempts per service for debugging concurrent execution

### Why Not NATS?

1. **Architectural mismatch**: Uncloud is explicitly AP-leaning; NATS JetStream KV is CP
2. **Operational overhead**: Requires running and maintaining additional infrastructure
3. **Partition behavior**: Lock acquisition during partition is undefined and dangerous
4. **Simpler alternatives exist**: Idempotency solves the problem without coordination
5. **Design philosophy**: "All machines are equal" conflicts with centralized lock service

### When Would NATS Make Sense?

NATS might be justified if Uncloud evolves to support:
- Multi-step workflows requiring exactly-once execution guarantees
- Cross-service transactions (deploy service A, then B, then C atomically)
- Cluster-wide leader election for background reconciliation loops
- Request-reply patterns for synchronous cross-machine communication

None of these are current requirements based on the codebase analysis.

---

## Files Referenced

| File | Line | Relevance |
|------|------|-----------|
| `pkg/client/deploy/deploy.go` | 65-102, 144-151 | Plan and Run methods, TODO acknowledgment |
| `pkg/client/deploy/operation.go` | 41-53, 166-172 | Container creation, sequential execution |
| `pkg/client/deploy/strategy.go` | 141-187, 354 | Replicated planning, random service ID |
| `pkg/client/container.go` | 37-41 | Random container suffix generation |
| `internal/machine/store/schema.sql` | 19-31 | Container table schema, no uniqueness constraint |
| `misc/design.md` | 49-122 | AP architecture, consensus "on demand" |
| `internal/corrosion/client.go` | 1-82 | Corrosion API with retry backoff |

---

*Assessment completed: 2025-01-24*
*Based on commit: Current HEAD of branch claude/assess-nats-deploy-lock-3AlFw*
