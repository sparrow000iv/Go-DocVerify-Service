# DocVerify — Cloud-Native Document Verification Service (Go)

A production-shaped Go microservice exposing the **same domain logic over both REST
and gRPC**, containerised with Docker, deployed to Kubernetes, instrumented with
Prometheus/Grafana, and validated by a layered test suite (unit, race, integration,
benchmark, smoke) wired into GitHub Actions CI/CD.

**Verified metrics (reproduce with `make ci`):**

| Metric | Value |
|---|---|
| Total test coverage | **78.4%** |
| Ginkgo integration specs | **23 passing** |
| Smoke test assertions | **16 passing** |
| Race detector | **Clean** (`-race` on all packages) |
| `Score()` benchmark | **100.6 ns/op, 0 allocs/op** |
| `Submit()` benchmark | **1297 ns/op, 3 allocs/op** |
| Kubernetes resources | **13** across 6 manifests |

---

## Why this project exists

It is built to demonstrate, end to end, the exact stack a cloud-native Go role
asks for: Golang backend development, Kubernetes, containerisation, REST + gRPC,
automated testing (including Ginkgo/Gomega), CI/CD, and observability.

## Architecture

```
                    ┌──────────────────────────┐
   REST :8080 ─────▶│                          │
                    │   internal/store         │
   gRPC :9090 ─────▶│   (RWMutex-guarded,      │
                    │    race-tested)          │
   /metrics ───────▶│                          │
                    └──────────────────────────┘
                                │
                    Prometheus ──▶ Grafana
```

Both transports are thin adapters over one domain core, so the APIs can never
drift apart — an invariant asserted directly by the cross-transport specs.

```
cmd/server/           Entry point: dual servers, graceful shutdown
internal/store/       Domain logic, concurrency-safe repository
internal/httpapi/     REST transport (net/http, Go 1.22+ routing)
internal/grpcapi/     gRPC transport, domain error → status code mapping
internal/metrics/     Prometheus collectors
proto/docverify/v1/   Protocol Buffers contract + generated stubs
test/integration/     Ginkgo/Gomega BDD suite
deploy/k8s/           Deployment, Service, ConfigMap, HPA, Prometheus, Grafana
.github/workflows/    Six-stage CI/CD pipeline
```

## Quick start

```bash
make test          # unit tests with the race detector
make integration   # Ginkgo integration suite
make cover         # coverage report
make run           # start locally on :8080 (REST) and :9090 (gRPC)
make smoke         # 16-assertion end-to-end smoke test
```

Deploy to a local Kubernetes cluster:

```bash
make kind-up       # create the kind cluster
make deploy        # build image, load into kind, apply manifests
make smoke         # verify through the cluster
make logs          # tail structured JSON logs
```

## API

### REST

| Method | Path | Description |
|---|---|---|
| `GET` | `/healthz` | Liveness probe |
| `GET` | `/readyz` | Readiness probe |
| `GET` | `/metrics` | Prometheus metrics |
| `POST` | `/api/v1/documents` | Submit a document |
| `GET` | `/api/v1/documents` | List (`?status=`, `?limit=`) |
| `GET` | `/api/v1/documents/{id}` | Fetch by id |
| `POST` | `/api/v1/documents/{id}/verify` | Run verification |
| `DELETE` | `/api/v1/documents/{id}` | Delete by id |

```bash
curl -X POST localhost:8080/api/v1/documents \
  -H 'Content-Type: application/json' \
  -d '{"owner":"tushar","doc_type":"passport","content":"payload"}'
```

```json
{
  "id": "doc-000001",
  "owner": "tushar",
  "doc_type": "passport",
  "status": "PENDING",
  "score": 0,
  "created_at": "2026-07-28T18:05:25Z",
  "updated_at": "2026-07-28T18:05:25Z"
}
```

### gRPC

Five RPCs — `Submit`, `Get`, `List`, `Verify`, `Delete` — defined in
[`docverify.proto`](proto/docverify/v1/docverify.proto). Server reflection is
enabled, so the running pod is explorable with `grpcurl`:

```bash
grpcurl -plaintext localhost:9090 list
grpcurl -plaintext -d '{"owner":"t","doc_type":"passport","content":"x"}' \
  localhost:9090 docverify.v1.DocVerify/Submit
```

## Testing strategy

Five layers, each catching a different class of defect:

1. **Table-driven unit tests** — validation matrix, lifecycle transitions,
   idempotency, sorting, filter/limit semantics.
2. **Race detector** — 50 concurrent goroutines hammering the store; CI fails on
   any detected data race.
3. **HTTP tests** — `httptest` against the real router: status codes, JSON
   content type, malformed bodies, unknown fields, 404/400/405 paths.
4. **gRPC tests** — in-process `bufconn` transport (no real ports), asserting
   correct `codes.NotFound` / `codes.InvalidArgument` mapping.
5. **Ginkgo/Gomega integration suite** — 23 BDD specs including
   **cross-transport consistency** (a document created over REST is readable over
   gRPC and vice versa) and concurrent load from both transports at once.

Plus benchmarks with allocation tracking and a 16-assertion smoke script.

### Error mapping

| Domain error | HTTP | gRPC |
|---|---|---|
| `ErrNotFound` | 404 | `NOT_FOUND` |
| `ErrInvalidOwner` / `ErrInvalidType` / `ErrUnsupported` | 400 | `INVALID_ARGUMENT` |
| unexpected | 500 | `INTERNAL` |

## Observability

Custom collectors exposed on `/metrics`:

- `docverify_http_requests_total{method,route,status}`
- `docverify_http_request_duration_seconds{method,route}` (histogram)
- `docverify_grpc_requests_total{method,code}`
- `docverify_grpc_request_duration_seconds{method}` (histogram)
- `docverify_documents_verified_total{status}`
- `docverify_documents_stored` (gauge)

Useful PromQL:

```promql
# p95 latency by route
histogram_quantile(0.95,
  sum(rate(docverify_http_request_duration_seconds_bucket[5m])) by (le, route))

# error rate
sum(rate(docverify_http_requests_total{status=~"5.."}[5m]))
  / sum(rate(docverify_http_requests_total[5m]))
```

Logs are structured JSON via `log/slog`, ready for any log aggregator.

## Production practices demonstrated

- **Graceful shutdown** on SIGTERM with a 15s drain, so rolling updates lose no
  in-flight requests.
- **Distroless non-root image**, read-only root filesystem, all capabilities
  dropped, static CGO-free binary.
- **Liveness and readiness probes** — readiness gates traffic during rollouts.
- **Resource requests/limits** plus an HPA scaling 2→8 replicas at 70% CPU.
- **`maxUnavailable: 0`** rolling strategy for zero-downtime deploys.
- **Tests run inside the Docker build**, so a broken image can never be produced.

## CI/CD pipeline

Six stages in `.github/workflows/ci.yml`:

1. **lint** — `gofmt` check, `go vet`, `staticcheck`
2. **unit-tests** — race detector + **70% coverage gate** (build fails below it)
3. **integration-tests** — Ginkgo with `--randomize-all` to catch order dependence
4. **benchmarks** — performance tracked as a build artifact
5. **docker** — buildx with layer caching, then a container smoke test
6. **k8s-deploy** — spins up a real kind cluster, applies manifests, waits for
   rollout, verifies live endpoints, dumps diagnostics on failure

## Tech stack

Go 1.23 · gRPC · Protocol Buffers · Docker (multi-stage, distroless) ·
Kubernetes · kind · Prometheus · Grafana · Ginkgo v2 · Gomega · GitHub Actions
