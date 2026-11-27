# ARIMA Strategy Deep Dive

The ARIMA strategy forecasts CPU and memory usage with (seasonal) ARIMA models from [goarima](https://github.com/sartorproj/goarima) and sizes each container for the **upper bound of the prediction interval** over a forecast horizon, rather than for the point forecast. Workloads with a trend are sized for where usage is heading, and workloads with daily or weekly cycles are sized for the next peak.

## How It Works

### 1. Usage History

Unlike the percentile strategy, which only needs P95/P99 values, the ARIMA strategy needs the usage history. When a Tailoring's FitProfile uses `strategy: arima`, the controller runs two extra range queries per analysis (CPU and memory for all containers of the workload). They use the same per-container expressions as the percentile queries:

- CPU: `max by (container) (rate(container_cpu_usage_seconds_total[5m]))`, in cores
- Memory: `max by (container) (container_memory_working_set_bytes)`, in bytes

The history covers the analysis window at a 5m resolution. For windows longer than about 34 days the step is coarsened, which keeps each series below 10,000 samples. If the history query fails, the analysis still succeeds with the percentile values.

### 2. Preparation

Before fitting, each series is:

1. **Cleaned**: samples that are NaN, infinite or negative are dropped, and samples are sorted by time.
2. **Downsampled**: samples are grouped into buckets of a common interval so that at most `maxSamples` (default 1000) remain. Each bucket keeps its **maximum**, so short peaks are not averaged away. The interval is chosen to divide a day where possible, so seasonal periods are a whole number of samples. For example, 7 days at 5m (2016 samples) become 672 samples at 15m, and 28 days at 5m (8064 samples) become 896 samples at 45m.
3. **Gap-filled**: missing buckets are filled by linear interpolation. If more than 20% are missing, the resource falls back to the percentile strategy.

### 3. Model Selection

In the default `mode: auto`, the order is selected automatically: differencing is chosen with stationarity tests (KPSS and ADF), the best non-seasonal and the best seasonal order are found by a stepwise search on AICc, and the candidate with the lower forecast error (RMSE) on the last 20% of the series is used. With `seasonal: true` (default), seasonal models are considered for:

- a **daily** period: `24h / interval` samples
- a **weekly** period: `168h / interval` samples

A period is only considered when the history covers at least three cycles. Daily seasonality therefore needs at least 3 days of history, and weekly seasonality an analysis window of at least 21 days. The seasonal model uses the period with the strongest autocorrelation (at least 0.4); other autocorrelation peaks between the daily and the weekly period (for example two days) are candidates as well. Without a clear seasonal pattern only non-seasonal models are fitted.

The search space is capped to bound the cost per analysis: `p, q <= 3`, `d <= 1`, and seasonal `P, Q, D <= 1`. Capping `d` at 1 avoids quadratic extrapolation of short-term noise.

With `mode: fixed`, the model `ARIMA(p,d,q)` or `SARIMA(p,d,q)(P,D,Q)[s]` is fitted as configured. The seasonal period `s` is given as a duration (`seasonalPeriod`) and converted to samples at the series interval.

### 4. Recommendation

The model forecasts `horizon / interval` steps ahead. The recommendation uses the largest upper bound over the horizon:

- **requests**: upper bound of the `confidence` (default 95%) prediction interval
- **limits**: upper bound of the `limitsConfidence` (default 99%) prediction interval

These two values take the place of P95 and P99 in the percentile calculation. Everything after that is identical to the percentile strategy: `requestsBufferPercent` / `limitsBufferPercent` are added (default 20% / 30%), each change is limited to `maxChangePercent` of the current value (default 50%), and safety rails and ResourceQuota limits are applied. A limit that ends up below its request is raised to the request. CPU is rounded up to whole millicores and memory to whole bytes, so recommendations are always valid, non-negative Kubernetes quantities.

### 5. Fallback

CPU and memory are forecast independently. A resource uses the **percentile strategy's result** when:

- no usage history is available (for example, the history query failed)
- the history has fewer than 48 samples or covers less than the horizon
- the series is constant (including all zeros)
- more than 20% of the samples are missing
- no model can be fitted, or the fit fails
- the forecast is NaN or infinite, is not positive, or exceeds 10 times the observed peak (a diverging fit)

The reason is recorded in the recommendation metadata, for example `cpu_fallback: "percentile: usage history is constant"`. When a forecast is used, the metadata contains `cpu_model` (e.g. `SARIMA(1,0,0)(1,1,0)[96]`), `cpu_sample_interval`, `cpu_samples`, `cpu_upper_bound` and `cpu_limits_upper_bound` (and the same keys for memory), plus `horizon`.

### On-Demand Forecasts

The API server exposes the same forecast for any container at `GET /api/v1/prometheus/forecast`, with the history, the point forecast and the prediction interval, so it can be shown in the UI. See the [API Reference](api-reference.md#container-usage-forecast).

## Parameters

All parameters are optional. Unknown parameters are rejected.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `mode` | string | `auto` | `auto` selects the order automatically, `fixed` uses `order` / `seasonalOrder` |
| `seasonal` | bool | `true` | `auto` only: also consider seasonal models with daily and weekly periods |
| `order` | [p, d, q] | | `fixed` only, required: non-seasonal order; `p, q <= 5`, `d <= 2` |
| `seasonalOrder` | [P, D, Q] | `[0, 0, 0]` | `fixed` only: seasonal order; `P, Q <= 2`, `D <= 1` |
| `seasonalPeriod` | duration | | `fixed` only: seasonal period `s` (e.g. `24h`), required with a non-zero `seasonalOrder`; at most `168h` |
| `confidence` | number | `0.95` | Prediction interval level for requests, between 0.5 and 0.999 |
| `limitsConfidence` | number | `0.99` | Prediction interval level for limits, between 0.5 and 0.999, at least `confidence` |
| `horizon` | duration | `24h` | Forecast horizon, at most `168h` |
| `maxSamples` | integer | `1000` | Maximum samples a model is fitted to (100 to 5000) |
| `requestsBufferPercent` | number | `20` | Buffer added to the requests upper bound (0 to 1000) |
| `limitsBufferPercent` | number | `30` | Buffer added to the limits upper bound (0 to 1000) |
| `maxChangePercent` | number | `50` | Maximum change per recommendation (0 to 1000) |

The default horizon of one day covers a full daily cycle, so the recommendation also covers the next daily peak.

## Configuration Examples

The snippets below are `FitProfile` specs; reference the FitProfile from a Tailoring with `spec.fitProfileRef`. A complete example is in `config/samples/fitprofile-arima.yaml`.

### Automatic Model Selection (Default)

```yaml
spec:
  strategy: arima
```

### Weekly Patterns

Weekly seasonality needs three weeks of history, so use a longer analysis window on the Tailoring (or the Atelier's `defaultAnalysisWindow`):

```yaml
# FitProfile
spec:
  strategy: arima
  parameters:
    horizon: 24h
---
# Tailoring
spec:
  analysisWindow: 672h # 28 days
```

### Fixed SARIMA Order

```yaml
spec:
  strategy: arima
  parameters:
    mode: fixed
    order: [1, 0, 1]
    seasonalOrder: [1, 1, 0]
    seasonalPeriod: 24h
```

### More Conservative Limits

```yaml
spec:
  strategy: arima
  parameters:
    confidence: 0.95
    limitsConfidence: 0.999
    limitsBufferPercent: 50
```

## When to Use the ARIMA Strategy

**Use ARIMA when:**
- Usage has a trend (growing or shrinking) that percentiles of the past would miss
- Usage follows daily or weekly cycles
- You want recommendations sized for a forecast horizon with a stated confidence

**Use Percentile when:**
- Usage is stable or history is short
- Usage has rare, very large spikes: the prediction interval assumes Gaussian errors, so a few extreme samples widen it considerably. Limit the effect with safety rails or `maxChangePercent`.

Prediction intervals of differenced models (`d = 1` or `D = 1`) widen with the horizon, so trending workloads receive more headroom for longer horizons.

## Performance Considerations

Fitting is bounded by `maxSamples` and the capped search space, and is deterministic for the same input. Measured fit time per resource (CPU or memory) on a single core of a recent x86 server (`go test -bench ARIMA ./internal/recommender/strategy/`):

| History | Samples fitted | Time |
|---------|----------------|------|
| 7 days at 5m, daily seasonal with trend | 672 at 15m | ~2 ms |
| 28 days at 5m, daily and weekly seasonal | 896 at 45m | ~4 ms |
| 28 days at 5m, `maxSamples: 5000` | 4032 at 10m | ~36 ms |

Each container costs two fits per analysis. The extra range queries return up to 10,000 samples per container and resource, so a 7 day window at 5m transfers about 2,000 samples per series.

## References

- [goarima](https://github.com/sartorproj/goarima)
- [Forecasting: Principles and Practice, Chapter 9: ARIMA models](https://otexts.com/fpp3/arima.html)
