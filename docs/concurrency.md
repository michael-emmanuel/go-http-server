# Concurrency

## 1. Mental model

`net/http` handles connections concurrently: one goroutine per connection, and
therefore many `ServeHTTP` calls running at the same time. Anything a handler can
reach, whether a field on the handler struct, a map, a counter, or a global, is
shared between requests unless proved otherwise.

Go's memory model gives two ways to make sharing safe: **synchronize** access
(mutex, atomic) or **avoid sharing** (each goroutine owns its data; communicate by
channel or by disjoint writes followed by a join). The right choice depends on
the shape of the data.

## 2. Code example

### Bounded fan-out (`domain.Service.process`)

```go
sem := make(chan struct{}, s.cfg.BatchConcurrency)
for i := 0; i < req.Items; i++ {
    select {
    case sem <- struct{}{}:        // acquire a slot; blocks at the limit
    case <-ctx.Done(): break launch // stop launching if the request is gone
    }
    wg.Add(1)
    go func(i int) {
        defer wg.Done()
        defer func() { <-sem }()   // release the slot
        v, err := s.runItem(ctx, i, req.ItemDelay)
        if err != nil { fail(err); return }
        values[i] = v              // disjoint write: no lock needed
        s.itemsProcessed.Add(1)    // shared counter: atomic
    }(i)
}
wg.Wait()                          // join: makes values[] safe to read
```

Why each primitive is there:

| Primitive                     | Why this one                                                                                                                        |
| ----------------------------- | ----------------------------------------------------------------------------------------------------------------------------------- |
| Buffered channel as semaphore | Bounds concurrent work. The launcher blocks at the limit and can also select on `ctx.Done()`. A mutex cannot do "at most N".        |
| `sync.WaitGroup`              | We need "wait for all", not a value from each.                                                                                      |
| Disjoint slice writes         | Each goroutine writes only `values[i]`; `wg.Wait()` establishes happens-before for the reader. A mutex here would be pure overhead. |
| `sync.Once` + `cancel()`      | Record only the _first_ error and cancel siblings exactly once. Later errors are almost always the resulting `context.Canceled`.    |
| `atomic.Int64`                | An independent counter written by many goroutines.                                                                                  |

### Why not a goroutine per item, unbounded

Goroutines are cheap, not free, and what they _touch_ (CPU, connections to
downstream services) is finite. With `items` caller-controlled, an unbounded
fan-out lets one request starve every other. The limit turns that into queueing
inside one request. Conversely, a limit of 1 would serialize independent I/O
waits and make the endpoint pointless. `WORK_BATCH_CONCURRENCY` is the knob.

### The incorrect version

```go
var count int                    // BAD: shared, unsynchronized

func handler(w http.ResponseWriter, r *http.Request) {
    count++                      // read-modify-write; not atomic
}
```

`count++` compiles to load, add, store. Two goroutines can both load 5 and both
store 6: one increment is lost. Worse, the compiler and CPU may reorder or cache
the value, so the program has undefined behavior under the Go memory model, not
merely "occasionally off by one". The race detector reports it.

Fix, for an independent counter:

```go
var count atomic.Int64
count.Add(1)                     // indivisible read-modify-write
```

When a mutex is the better tool: when several values must change **together**
(a map and its eviction-order slice in `domain.Store`; "check then act" like
"if not present, insert"), or when the critical section is a compound
operation. Atomics protect one word; they cannot make two words consistent with
each other. That is also why `Metrics.Snapshot` is documented as approximate:
each field is read atomically but not as one atomic cut.

### Channels: when

Use a channel to transfer ownership or signal an event: `health.Draining()`
(closed once, observed by anyone), the worker queue (`chan Task`), and the
semaphore above. Don't use one to protect shared state; a mutex says what it
means.

## 3. Production implications

- **Send-on-closed-channel panics.** `worker.Pool.Submit` sends on `p.tasks`
  while `Shutdown` closes it. `Submit` holds `RLock` and `Shutdown` takes `Lock`
  to set `closed` and close, so a send can never race a close.
  `TestConcurrentSubmitAndShutdown` and a mutation test guard it.
- **Panic containment.** Panics in goroutines you start crash the process.
  `runItem` and `runTask` recover and convert to errors.
- **No goroutine leaks by construction.** `RunBatch` does not return until every
  goroutine has exited (`wg.Wait`), `Pool.Shutdown` waits for its workers even
  after cancelling them, and `Serve` reaps its goroutine. Each of these has a
  test that fails if the join is removed.
- **Backpressure.** `Pool.Submit` never blocks; a full queue returns
  `ErrQueueFull`, becoming `503` with `Retry-After`. Blocking would just move the
  unbounded queue into parked HTTP handler goroutines.

## 4. Common mistake

Using a `map` from handlers with no lock. Unlike a lost increment, this can
crash the whole process: the runtime detects concurrent map writes and
terminates with `fatal error: concurrent map writes`, which `recover` cannot
catch. `domain.Store` guards its map with a `sync.RWMutex`, and
`TestStoreConcurrentAccess` exercises it under `-race`.

A different mistake, which we made while testing this repo: a race test that
_passes_ because it accidentally synchronizes. Calling an atomic operation on a
shared variable before the plain access created a happens-before edge, so a
non-atomic counter was invisible to the detector. The counter test now calls
only `Observe`. If a race test can't fail when you break the code, it proves
nothing; see the mutation notes in the README.

## 5. What happens under load

- More concurrent requests means more goroutines competing for the same
  `BATCH_CONCURRENCY` slots _per request_, not globally. Total in-flight work is
  bounded by (concurrent requests x per-request limit). If that product exceeds
  what downstream systems tolerate, add a global limiter; this repo does not.
- Atomics under heavy contention still bounce a cache line between cores. At very
  high request rates a sharded counter is the next step. `BenchmarkObserveParallel`
  lets you measure before deciding.
- The job queue fills, `Submit` starts returning `ErrQueueFull`, and clients see 503. Latency for accepted jobs stays bounded by queue length divided by worker
  throughput.
