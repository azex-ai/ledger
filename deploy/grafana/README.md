# Grafana dashboard

`ledger-dashboard.json` is an importable Grafana dashboard (UID
`ledger-ops`) covering the ledgerd Prometheus metrics: money-path rate and
latency, reservations, worker health (rollup queue, checkpoint age), event
delivery, and integrity signals (reconcile failures, balance drift,
idempotency collisions).

Import: Grafana → Dashboards → Import → upload the JSON, pick your
Prometheus datasource (the dashboard exposes it as the `datasource`
variable).

Panels map to the incident scenarios in
[`docs/RUNBOOK.md`](../../docs/RUNBOOK.md); the corresponding alert rules
ship in the Helm chart (`metrics.prometheusRules.enabled=true`).
