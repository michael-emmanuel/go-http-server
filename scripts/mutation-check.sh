#!/usr/bin/env bash
# Manual mutation testing: for each mutant, copy the repository to a scratch
# directory, break one behavior, and confirm the test suite fails.
#
# A mutant that SURVIVES means the tests would not notice that regression.
# A mutant that is INVALID (target text not found, or it does not compile)
# means this script has drifted from the source and needs updating.
#
# Not run in CI: it takes several minutes and its search strings are tied to the
# exact source text. Run it after touching concurrency, shutdown, or middleware:
#
#   make mutation                     # everything
#   FILTER=shutdown make mutation     # only mutants whose name contains "shutdown"
set -uo pipefail
FILTER="${FILTER:-}"

SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
killed=0; survived=0; invalid=0

run_mut() { # name file old new packages
  local name="$1" file="$2" old="$3" new="$4" pkgs="$5"
  [[ -n "$FILTER" && "$name" != *"$FILTER"* ]] && return
  rm -rf "$WORK/m" && cp -r "$SRC" "$WORK/m" && cd "$WORK/m" || exit 2
  if ! python3 - "$file" "$old" "$new" <<'PY'
import sys
f, old, new = sys.argv[1:4]
s = open(f).read()
if old not in s:
    sys.exit(1)
open(f, "w").write(s.replace(old, new, 1))
PY
  then echo "INVALID  : $name (target text not found)"; invalid=$((invalid+1)); return; fi
  if ! go build ./... 2>"$WORK/build.err"; then
    echo "INVALID  : $name (mutant does not compile: $(sed -n 2p "$WORK/build.err" | cut -c1-80))"; invalid=$((invalid+1)); return
  fi
  local out; out=$(go test -race -count=1 -timeout 30s $pkgs 2>&1)
  if grep -qE '^(--- FAIL|panic:)|WARNING: DATA RACE|timed out' <<<"$out"; then
    echo "killed   : $name"; killed=$((killed+1))
  else
    echo "SURVIVED : $name   <-- tests did not detect this"; survived=$((survived+1))
  fi
}

# A plain (non-atomic) counter must be caught by the race detector. This mutant
# needs several edits at once, so it does not go through run_mut.
mutate_counter() {
  [[ -n "$FILTER" && "metrics: non-atomic request counter" != *"$FILTER"* ]] && return
  rm -rf "$WORK/m" && cp -r "$SRC" "$WORK/m" && cd "$WORK/m" || exit 2
  python3 - <<'PY'
p = "internal/metrics/metrics.go"
s = open(p).read()
s = s.replace("requestsTotal atomic.Int64", "requestsTotal int64")
s = s.replace("m.requestsTotal.Add(1)", "m.requestsTotal++").replace("m.requestsTotal.Load()", "m.requestsTotal")
open(p, "w").write(s)
PY
  # Capture first: under pipefail, `go test | grep -q` reports failure when grep
  # exits early and go test receives SIGPIPE, which would misreport a kill.
  local out; out=$(go test -race -count=1 ./internal/metrics 2>&1)
  if grep -qE 'DATA RACE|--- FAIL' <<<"$out"; then
    echo "killed   : metrics: non-atomic request counter (race detector)"; killed=$((killed+1))
  else
    echo "SURVIVED : metrics: non-atomic request counter"; survived=$((survived+1))
  fi
}
mutate_counter

run_mut "domain: first error no longer cancels siblings" internal/domain/service.go \
'firstErr = err
			cancel()' 'firstErr = err' ./internal/domain
run_mut "domain: wg.Wait removed" internal/domain/service.go \
'	wg.Wait()

	if firstErr' '	if firstErr' ./internal/domain
run_mut "domain: concurrency limit ignored" internal/domain/service.go \
  'sem := make(chan struct{}, s.cfg.BatchConcurrency)' 'sem := make(chan struct{}, req.Items)' ./internal/domain
run_mut "domain: panic recovery removed" internal/domain/service.go \
'if r := recover(); r != nil {
			s.log.ErrorContext(ctx, "work item panicked"' 'if r := any(nil); r != nil {
			s.log.ErrorContext(ctx, "work item panicked"' ./internal/domain
run_mut "domain: rejected job not rolled back" internal/domain/service.go \
  's.store.Delete(id) // roll back' '// s.store.Delete(id) // roll back' ./internal/domain
run_mut "worker: Submit without lock" internal/worker/worker.go \
'	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return ErrClosed
	}
	select {' '	select {' ./internal/worker
run_mut "worker: deadline does not cancel tasks" internal/worker/worker.go \
'	case <-ctx.Done():
		p.cancel()
		<-done' '	case <-ctx.Done():
		<-done' ./internal/worker
run_mut "worker: task panic kills worker" internal/worker/worker.go \
'if r := recover(); r != nil {
			p.log.Error("worker task panicked"' 'if r := any(nil); r != nil {
			p.log.Error("worker task panicked"' ./internal/worker
run_mut "httpapi: router loses path values" internal/httpapi/routes.go \
'rt.mux.ServeHTTP(w, r)
		return
	}

	// No pattern' 'h.ServeHTTP(w, r)
		return
	}

	// No pattern' ./internal/httpapi
run_mut "httpapi: internal error text leaked" internal/httpapi/response.go \
  'message: "internal server error", level: slog.LevelError}' 'message: err.Error(), level: slog.LevelError}' ./internal/httpapi
run_mut "httpapi: no body size limit" internal/httpapi/response.go \
  'r.Body = http.MaxBytesReader(w, r.Body, maxBytes)' '_ = maxBytes' ./internal/httpapi
run_mut "httpapi: unknown JSON fields allowed" internal/httpapi/response.go \
  'dec.DisallowUnknownFields()' '' ./internal/httpapi
run_mut "httpapi: trailing JSON allowed" internal/httpapi/response.go \
  'if _, err := dec.Token(); !errors.Is(err, io.EOF) {' 'if _, err := dec.Token(); false && !errors.Is(err, io.EOF) {' ./internal/httpapi
run_mut "httpapi: shutdown cause ignored" internal/httpapi/response.go \
  'errors.Is(context.Cause(r.Context()), domain.ErrUnavailable)' 'false' ./internal/httpapi
run_mut "httpapi: DeadlineExceeded mapped after net timeout" internal/httpapi/response.go \
'case errors.Is(err, context.DeadlineExceeded):
		return failure{status: http.StatusGatewayTimeout' 'case false:
		return failure{status: http.StatusGatewayTimeout' ./internal/httpapi
run_mut "httpapi: duration overflow unchecked" internal/httpapi/handlers.go \
  'if ms < 0 || int64(ms) > maxMS {' 'if false && (ms < 0 || int64(ms) > maxMS) {' ./internal/httpapi
run_mut "server: Recovery moved outside AccessLog" internal/server/server.go \
'AccessLog(log),
		Metrics(m),
		Recovery(log),' 'Recovery(log),
		AccessLog(log),
		Metrics(m),' ./internal/server
run_mut "server: RequestID not outermost" internal/server/server.go \
'RequestID(),
		AccessLog(log),' 'AccessLog(log),
		RequestID(),' ./internal/server
run_mut "shutdown: readiness not flipped" internal/server/shutdown.go \
'	s.health.MarkDraining()

	// The shutdown' '	// The shutdown' ./test/...
run_mut "shutdown: cancel phase skipped" internal/server/shutdown.go \
'		s.baseCancel(domain.ErrUnavailable)
		if err := s.http.Shutdown(ctx); err != nil {' '		_ = domain.ErrUnavailable
		if err := context.DeadlineExceeded; err != nil {' ./test/...
run_mut "shutdown: cancel cause dropped" internal/server/shutdown.go \
'		s.baseCancel(domain.ErrUnavailable)
		if err := s.http.Shutdown(ctx); err != nil {' '		_ = domain.ErrUnavailable
		s.baseCancel(nil)
		if err := s.http.Shutdown(ctx); err != nil {' ./test/...
run_mut "shutdown: force close missing" internal/server/shutdown.go \
'			if cerr := s.http.Close(); cerr != nil {
				errs = append(errs, fmt.Errorf("force close: %w", cerr))
			}' '' ./test/...
run_mut "shutdown: worker pool not drained" internal/server/shutdown.go \
'	if err := s.pool.Shutdown(ctx); err != nil {
		errs = append(errs, err)
	}' '' ./test/...
run_mut "shutdown: Shutdown() replaced by Close()" internal/server/shutdown.go \
  'if err := s.http.Shutdown(drainCtx); err != nil {' 'if err := s.http.Close(); err != nil {' ./test/...
run_mut "health: draining can return to ready" internal/health/health.go \
  'c.state.CompareAndSwap(int32(StateStarting), int32(StateReady))' \
  'c.state.CompareAndSwap(int32(StateDraining), int32(StateReady)) || c.state.CompareAndSwap(int32(StateStarting), int32(StateReady))' ./internal/health
run_mut "middleware: auth accepts any token" internal/server/middleware.go \
  '!ok || subtle.ConstantTimeCompare([]byte(got), want) != 1' '!ok || subtle.ConstantTimeCompare([]byte(got), want) < 0' ./internal/server
run_mut "middleware: request ID unvalidated" internal/server/middleware.go \
  'if !requestid.Valid(id) {' 'if id == "" {' ./internal/server
run_mut "middleware: request timeout not released" internal/server/middleware.go \
  'defer cancel() // release the timer as soon as the handler returns' '_ = cancel' ./internal/server
run_mut "middleware: query string logged" internal/server/middleware.go \
  'slog.String("path", r.URL.Path),' 'slog.String("path", r.URL.RequestURI()),' ./internal/server
run_mut "config: request timeout may equal write timeout" internal/config/config.go \
  'check(c.RequestTimeout < c.WriteTimeout,' 'check(true || c.RequestTimeout < c.WriteTimeout,' ./internal/config
run_mut "config: drain delay unbounded" internal/config/config.go \
  'check(c.ShutdownDrainDelay <= c.ShutdownTimeout/2,' 'check(true || c.ShutdownDrainDelay <= c.ShutdownTimeout/2,' ./internal/config
run_mut "config: auth token in log value" internal/config/config.go \
  'slog.Bool("auth_enabled", c.AuthToken != ""),' 'slog.String("auth_token", c.AuthToken),' ./internal/config

echo
echo "killed: $killed   survived: $survived   invalid: $invalid"
[ "$survived" -eq 0 ] && [ "$invalid" -eq 0 ]
