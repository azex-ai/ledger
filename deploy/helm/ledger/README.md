# ledger Helm chart

Deploy the `ledgerd` HTTP service on Kubernetes. Migrations run automatically
on container startup; the chart assumes you provide a managed PostgreSQL 17
instance reachable from the cluster.

## Quick start

```bash
helm install ledger ./deploy/helm/ledger \
  --set databaseUrl='postgres://user:pass@db:5432/ledger?sslmode=require' \
  --set apiKeys='abc123,def456' \
  --set corsAllowedOrigin='https://app.example.com'
```

Production deployments should use an existing Secret instead of inlining the
DB URL:

```bash
kubectl create secret generic ledger-db --from-literal=DATABASE_URL=...
helm install ledger ./deploy/helm/ledger \
  --set existingSecret=ledger-db \
  --set apiKeys=$LEDGER_KEYS \
  --set corsAllowedOrigin=https://app.example.com
```

## Values

See `values.yaml` for the full list with comments.

| Key | Default | Description |
|-----|---------|-------------|
| `replicaCount` | `2` | Workers share rollup/expiration via SKIP LOCKED — multi-replica is safe. |
| `image.repository` | `ghcr.io/azex-ai/ledger` | Image. Override during release. |
| `databaseUrl` | `""` | Inline DB URL (creates a Secret). Use `existingSecret` in production. |
| `existingSecret` | `""` | Name of an existing K8s Secret holding `DATABASE_URL`. |
| `apiKeys` | `""` | Comma-separated bearer keys. **Required in production**. |
| `corsAllowedOrigin` | `""` | Required when `env != "dev"`. |
| `metrics.enabled` | `true` | Adds Prometheus scrape annotations. |
| `ingress.enabled` | `false` | Set to `true` and provide hosts to expose externally. |

## Verifying

After install:

```bash
kubectl logs deploy/ledger
# expect: "migrations applied" + "listening on :8080"

kubectl port-forward svc/ledger 8080:80
curl localhost:8080/api/v1/system/health
```

Then run `kubectl exec` into a pod and use `ledger-cli` (separate binary in
the same image) to spot-check reconciliation:

```bash
ledger-cli reconcile --full
```

## Notes

- The chart deploys ledgerd only. Bring your own Postgres (RDS, Cloud SQL,
  Crunchy operator, etc.) — running stateful Postgres in Kubernetes is out
  of scope here.
- Concurrency is safe across replicas: `pg_advisory_lock` + `SELECT FOR UPDATE
  SKIP LOCKED` prevents double-execution of background jobs.
- For zero-downtime upgrades, `kubectl rollout` strategy defaults are fine —
  the worker holds advisory locks scoped to the row being processed.
