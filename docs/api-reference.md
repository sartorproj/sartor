# API Reference

The Sartor server (`cmd/server`) serves a JSON REST API for the UI and other clients on `--addr` (default `:8080`).

## Conventions

**Authentication**: when API keys are configured (Secret `--api-key-secret-name` in `--api-key-secret-namespace`), every request except `/healthz` and `/readyz` must send a key in the `X-API-Key` header (or the `api_key` query parameter). A missing key returns `401`, an unknown key `403`.

**Responses** use a common envelope:

```json
{"success": true, "data": {}, "message": "optional"}
{"success": false, "error": "what went wrong"}
```

## Endpoints

| Path | Description |
|------|-------------|
| `GET /api/v1/tailorings`, `GET /api/v1/tailorings/{namespace}/{name}` | Tailorings (optional `namespace` filter on the list); see [Tailoring list](#tailoring-list) |
| `GET /api/v1/atelier` | Atelier summary |
| `GET /api/v1/dashboard/stats` | Dashboard statistics |
| `GET /api/v1/recommendations/{namespace}/{tailoring}` | Last analysis of a Tailoring |
| `GET /api/v1/analytics/savings` | [Request savings across all Tailorings](#savings), with optional cost figures |
| `GET /api/v1/analytics/top-gains` | [Containers with the largest reclaimable requests](#top-gains) |
| `GET /api/v1/fitprofiles`, `GET /api/v1/fitprofiles/{namespace}/{name}` | FitProfiles |
| `GET /api/v1/strategies`, `GET /api/v1/strategies/{name}` | Recommendation strategies and their parameter schemas |
| `GET /api/v1/metrics/{namespace}/{tailoring}` | P95/P99 usage of a Tailoring's containers |
| `GET /api/v1/prometheus/metrics` | [Usage time series of a container](#container-usage-time-series) |
| `GET /api/v1/prometheus/forecast` | [Usage forecast of a container](#container-usage-forecast) |
| `/api/v1/argocd/...` | ArgoCD applications, sync, refresh and detection (when enabled) |
| `/api/v1/opencost/...` | OpenCost allocations, summaries and analytics (when enabled) |

## Tailoring List

Each entry of `GET /api/v1/tailorings` carries, besides the target, FitProfile, PR and last analysis fields, the health of the analysis:

| Field | Description |
|-------|-------------|
| `readyCondition` | Status of the `Ready` condition (`True`, `False`, or empty) |
| `analyzedCondition` | Status of the `Analyzed` condition; absent before the first analysis attempt |
| `analyzedReason` | Reason of the `Analyzed` condition, for example `AnalysisComplete`, `FitProfileFallback`, `MetricsQueryError` or `NoMetrics` |
| `analyzedMessage` | Message of the `Analyzed` condition |
| `nextAnalysisTime` | When the next analysis is scheduled (RFC 3339, UTC); absent when none is scheduled |
| `consecutiveFailures` | Failed analyses since the last successful one (`0` when healthy); retries back off exponentially |

## Tailoring Status Fields

These Tailoring status fields are returned by `GET /api/v1/tailorings/{namespace}/{name}` and used by the analytics endpoints:

| Field | Description |
|-------|-------------|
| `status.lastAnalysis.replicas` | Desired replica count of the target at analysis time, read from kube-state-metrics in the same metrics backend as the usage (`kube_deployment_spec_replicas`, `kube_statefulset_replicas`, or `kube_daemonset_status_desired_number_scheduled` for a DaemonSet), scoped by the Atelier's `prometheus.clusterLabel` and `spec.target.cluster`. Unset (not `0`) when the series is missing or the query fails; `0` means the workload is scaled to zero. With an HPA it is the replica count at that moment. |
| `FitProfileResolved` condition | `True` (reason `FitProfileResolved`) when the referenced FitProfile was used as configured. `False` with reason `FitProfileFallback` when the analysis fell back: the FitProfile does not exist or cannot be read (the percentile strategy with default parameters is used), its `parameters` are not a JSON object (default parameters are used), or its `strategy` is unknown (the percentile strategy is used). The message says which. |
| `Analyzed` condition | After a successful analysis: reason `AnalysisComplete`, or `FitProfileFallback` with the fallback in the message when the FitProfile could not be used as configured. |

## Savings

`GET /api/v1/analytics/savings` estimates the resources the latest recommendations would free (or need) across all Tailorings, from Tailoring status only. It needs neither OpenCost nor a metrics query.

For every container of a Tailoring's last analysis that has both a current and a recommended request, the saving is `(current request - recommended request) x replicas`, per resource. Positive savings add to `reclaimable`, negative ones to `increase`, so an increase in one container never hides a decrease in another. Quantities in any unit (`250m`, `1.5`, `512Mi`, `1Gi`, `1048576Ki`, plain bytes) are converted to **cores** and **GiB** (2^30 bytes).

A Tailoring is included when it has a last analysis, a replica count (`status.lastAnalysis.replicas`) and at least one container with both a current and a recommended CPU or memory request. Otherwise it is counted as skipped, with the first reason that applies: `skippedNoAnalysis`, `skippedNoReplicas`, `skippedNoCurrentRequest`. In an included Tailoring, a resource without both requests (for example no CPU request set) is left out of that container's saving. Negative or implausibly large quantities (above 2^20 cores or 2^50 bytes) are treated as missing.

| Parameter | Required | Description |
|-----------|----------|-------------|
| `cpuPricePerCoreHour` | no | Price of one core for one hour. Non-negative number. |
| `memPricePerGiBHour` | no | Price of one GiB of memory for one hour. Non-negative number. |

There are no default prices: without a price parameter the response has no `cost` and no `pricing`. With only one of them, the cost covers only that resource. Prices are in whatever currency they are given in; multiply the hourly figures by 730 for an approximate month.

### Response

```json
{
  "success": true,
  "data": {
    "totals": {
      "cpu": {"reclaimableCores": 12.5, "increaseCores": 1.25, "netCores": 11.25},
      "memory": {"reclaimableGiB": 40, "increaseGiB": 2.5, "netGiB": 37.5},
      "cost": {"reclaimablePerHour": 0.7, "increasePerHour": 0.0625, "netPerHour": 0.6375}
    },
    "byCluster": [
      {"cluster": "example-cluster", "tailorings": 8, "cpu": {}, "memory": {}, "cost": {}}
    ],
    "byNamespace": [
      {"cluster": "example-cluster", "namespace": "example-namespace", "tailorings": 3, "cpu": {}, "memory": {}, "cost": {}}
    ],
    "tailorings": {"total": 10, "included": 8, "skippedNoAnalysis": 1, "skippedNoReplicas": 1, "skippedNoCurrentRequest": 0},
    "pricing": {"cpuPricePerCoreHour": 0.04, "memPricePerGiBHour": 0.005}
  }
}
```

| Field | Description |
|-------|-------------|
| `totals` | Sum over all included Tailorings |
| `byCluster` | One entry per `spec.target.cluster`, sorted by cluster. Tailorings without a cluster are grouped under `""`. |
| `byNamespace` | One entry per cluster and **workload** namespace (`spec.target.namespace`, or the Tailoring's namespace), sorted by cluster then namespace |
| `tailorings` (in groups) | Number of included Tailorings in the group |
| `cpu`, `memory` | `reclaimable*` sums the decreases, `increase*` the increases (both non-negative), `net*` is reclaimable minus increase |
| `cost` | Only with a price: the same three figures priced per hour |
| `tailorings` (top level) | How many Tailorings exist, were included and were skipped, by reason |
| `pricing` | The prices used; absent without a price parameter |

With no Tailorings the response is `200` with zero totals and empty `byCluster` and `byNamespace` lists.

### Status Codes

| Status | When |
|--------|------|
| `200` | Report, including an empty one |
| `400` | A price parameter is empty, not a number, negative, `NaN` or infinite |
| `405` | Method other than `GET` |
| `500` | Tailorings cannot be listed |

### Example

```bash
curl -H "X-API-Key: $API_KEY" \
  "http://localhost:8080/api/v1/analytics/savings?cpuPricePerCoreHour=0.04&memPricePerGiBHour=0.005"
```

## Top Gains

`GET /api/v1/analytics/top-gains` lists the containers with the largest reclaimable requests, computed exactly like the [savings](#savings) (`(current - recommended request) x replicas`, from included Tailorings only), in two rankings: by reclaimable CPU and by reclaimable memory. Containers without a positive saving for a resource are not in that resource's list. Ties are ordered by cluster, Tailoring namespace, Tailoring name and container.

| Parameter | Required | Default | Description |
|-----------|----------|---------|-------------|
| `limit` | no | `20` | Maximum entries per list; an integer from 1 to 200 |

### Response

```json
{
  "success": true,
  "data": {
    "limit": 20,
    "byCPU": [
      {
        "cluster": "example-cluster",
        "namespace": "example-namespace",
        "tailoring": "my-app",
        "workloadKind": "Deployment",
        "workloadName": "my-app",
        "workloadNamespace": "example-namespace",
        "container": "app",
        "replicas": 3,
        "currentRequests": {"cpu": "1", "memory": "1Gi"},
        "recommendedRequests": {"cpu": "500m", "memory": "512Mi"},
        "cpuSavingCores": 1.5,
        "memorySavingGiB": 1.5
      }
    ],
    "byMemory": [],
    "tailorings": {"total": 10, "included": 8, "skippedNoAnalysis": 1, "skippedNoReplicas": 1, "skippedNoCurrentRequest": 0}
  }
}
```

| Field | Description |
|-------|-------------|
| `namespace`, `tailoring` | The Tailoring (use with `GET /api/v1/tailorings/{namespace}/{name}`) |
| `workloadKind`, `workloadName`, `workloadNamespace`, `cluster` | The target workload |
| `currentRequests`, `recommendedRequests` | Per-pod requests as Kubernetes quantities; a resource without a request is absent |
| `replicas` | `status.lastAnalysis.replicas` |
| `cpuSavingCores`, `memorySavingGiB` | `(current - recommended) x replicas`: positive when reclaimable, negative when an increase is needed, absent when either request is missing |
| `tailorings` | As for the savings endpoint |

### Status Codes

| Status | When |
|--------|------|
| `200` | Rankings, possibly empty |
| `400` | `limit` is empty, not an integer, below 1 or above 200 |
| `405` | Method other than `GET` |
| `500` | Tailorings cannot be listed |

## Container Usage Time Series

`GET /api/v1/prometheus/metrics` returns the CPU or memory usage of one container over time, reduced with `max` over all pods of the workload, at about 200 points per window.

| Parameter | Required | Default | Description |
|-----------|----------|---------|-------------|
| `namespace` | yes | | Namespace of the workload |
| `name` | yes | | Workload name (`podNamePrefix` is accepted as a legacy alias) |
| `containerName` | yes | | Container name |
| `resourceType` | yes | | `cpu` or `memory` |
| `kind` | no | `Deployment` | `Deployment`, `StatefulSet` or `DaemonSet` |
| `window` | no | `1h` | Go duration (`30m`, `24h`, `168h`), more than 0 and at most `744h` (31 days) |
| `cluster` | when the Atelier sets `prometheus.clusterLabel` | | Value of the cluster label; must be empty when no cluster label is set |

Namespace, workload and container names must be valid Kubernetes names. Invalid or missing parameters return `400` without querying Prometheus.

`data` is a list of `{"timestamp": <unix seconds>, "value": <number>}`. CPU is in **cores**, memory in **MiB**.

## Container Usage Forecast

`GET /api/v1/prometheus/forecast` forecasts the CPU or memory usage of one container with the [ARIMA strategy](arima-strategy.md), on demand. It uses the same usage history, preparation, model selection and sanity checks as an analysis with `strategy: arima`, so the UI can show what the strategy would forecast for any container, whether or not a Tailoring uses the ARIMA strategy.

### Parameters

All parameters of the [usage time series](#container-usage-time-series) endpoint are accepted with the same meaning and validation, except that `window` defaults to `168h` (the history the model is fitted to). In addition:

| Parameter | Default | Description |
|-----------|---------|-------------|
| `horizon` | `24h` | Go duration to forecast ahead, more than 0 and at most `168h`, and not longer than `window` |
| `confidence` | `0.95` | Level of the prediction interval, greater than 0 and less than 1 |
| `mode` | `auto` | `auto` selects the order automatically, `fixed` uses `order` / `seasonalOrder` |
| `seasonal` | `true` | `auto` only: also consider daily and weekly seasonal models (`true` / `false`) |
| `order` | | `fixed` only, required: `p,d,q` (for example `1,1,1`); `p, q <= 5`, `d <= 2` |
| `seasonalOrder` | `0,0,0` | `fixed` only: `P,D,Q`; `P, Q <= 2`, `D <= 1` |
| `seasonalPeriod` | | `fixed` only: seasonal period as a Go duration (for example `24h`), required with a non-zero `seasonalOrder` |

The model parameters are validated exactly like the [FitProfile parameters](arima-strategy.md#parameters) of the same name.

### Response

```json
{
  "success": true,
  "data": {
    "history": [{"timestamp": 1700000000, "value": 412.5}],
    "forecast": [{"timestamp": 1700604900, "value": 430.1, "lower": 401.7, "upper": 458.6}],
    "level": 0.95,
    "model": "SARIMA(1,0,0)(1,1,0)[96]",
    "sampleInterval": "15m0s"
  }
}
```

| Field | Description |
|-------|-------------|
| `history` | Usage the forecast is based on, at the strategy's query resolution (5m). Samples that are NaN, infinite or negative are left out. |
| `forecast` | One point per `sampleInterval` covering `horizon`, starting one interval after the history: the point forecast `value` and the prediction interval `lower` / `upper` at `level`. All values are clamped at 0. Empty when there is no forecast. |
| `level` | The `confidence` used |
| `model` | Fitted model, for example `ARIMA(2,1,1)` or `SARIMA(1,0,0)(1,1,0)[96]`; empty when there is no forecast |
| `sampleInterval` | Interval of the forecast points (Go duration). The history is downsampled to at most 1000 samples before fitting, keeping the maximum of each bucket, so this is often coarser than the history. Without a forecast it is the history resolution. |
| `fallbackReason` | Present only when there is no forecast; explains why |

Units match the usage time series endpoint: CPU in **cores**, memory in **MiB**.

When the history cannot be forecast, the response is still `200` with `"success": true`, an empty `forecast` and a `fallbackReason`. Reasons are the strategy's [fallback conditions](arima-strategy.md#5-fallback), for example:

- `no usage history` (no samples for the container)
- `history too short: 12 valid samples, need 48`, or a history that covers less than the horizon
- `usage history is constant` (including all zeros)
- `history has too many gaps: 35% of samples missing`
- `model selection found no model that fits`, or a forecast that is not finite or diverges from the observed peak

### Status Codes

| Status | When |
|--------|------|
| `200` | Forecast, or no forecast with a `fallbackReason` |
| `400` | Missing or invalid parameter, `window` or `horizon` out of range, `window` shorter than `horizon`, or a missing (or unexpected) `cluster` for the Atelier's cluster label. Prometheus is not queried. |
| `405` | Method other than `GET` |
| `500` | No Atelier, Prometheus client or credentials cannot be set up, or the history query fails |
| `504` | The request did not finish within 12 seconds or was cancelled |

### Cost

A request runs two range queries (CPU and memory of all containers of the workload, as an analysis does) of at most 10,000 samples per series, and one model fit on at most 1000 samples, which takes a few milliseconds (see [Performance Considerations](arima-strategy.md#performance-considerations)). At most 4 fits run at a time per server; further requests wait for a free slot within their timeout.

### Example

```bash
curl -H "X-API-Key: $API_KEY" \
  "http://localhost:8080/api/v1/prometheus/forecast?namespace=example&name=api&containerName=app&resourceType=memory&window=336h&horizon=48h"
```
