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
| `GET /api/v1/tailorings`, `GET /api/v1/tailorings/{namespace}/{name}` | Tailorings (optional `namespace` filter on the list) |
| `GET /api/v1/atelier` | Atelier summary |
| `GET /api/v1/dashboard/stats` | Dashboard statistics |
| `GET /api/v1/recommendations/{namespace}/{tailoring}` | Last analysis of a Tailoring |
| `GET /api/v1/fitprofiles`, `GET /api/v1/fitprofiles/{namespace}/{name}` | FitProfiles |
| `GET /api/v1/strategies`, `GET /api/v1/strategies/{name}` | Recommendation strategies and their parameter schemas |
| `GET /api/v1/metrics/{namespace}/{tailoring}` | P95/P99 usage of a Tailoring's containers |
| `GET /api/v1/prometheus/metrics` | [Usage time series of a container](#container-usage-time-series) |
| `GET /api/v1/prometheus/forecast` | [Usage forecast of a container](#container-usage-forecast) |
| `/api/v1/argocd/...` | ArgoCD applications, sync, refresh and detection (when enabled) |
| `/api/v1/opencost/...` | OpenCost allocations, summaries and analytics (when enabled) |

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
