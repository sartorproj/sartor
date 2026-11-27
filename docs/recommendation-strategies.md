# Recommendation Strategies

Sartor supports multiple recommendation strategies through a flexible plugin system. This document describes available strategies and how to configure them.

## Available Strategies

### 1. Percentile Strategy (Default)

The **percentile** strategy uses P95 for requests and P99 for limits, with configurable buffers.

**Configuration** (in a FitProfile):
```yaml
apiVersion: autoscaling.sartorproj.io/v1alpha1
kind: FitProfile
metadata:
  name: percentile-balanced
spec:
  strategy: percentile
  parameters:
    requestsBufferPercent: 20
    limitsBufferPercent: 30
    maxChangePercent: 50
```

**How it works:**
- Calculates P95 and P99 from historical metrics
- Adds `requestsBufferPercent` and `limitsBufferPercent` on top (see the eco, balanced and critical samples in `config/samples`)
- Limits each change to `maxChangePercent` of the current value
- Respects safety rails and resource quotas
- Raises a limit that ends up below its request to the request (recorded as `cpu_limit_raised_to_request` / `memory_limit_raised_to_request` in the recommendation metadata)

**Best for:**
- Most production workloads
- Workloads with stable usage patterns
- Quick recommendations without complex analysis

### 2. DSP Strategy

The **DSP (Digital Signal Processing)** strategy uses FFT and autocorrelation to detect periodic patterns and predict future usage.

**Configuration** (in a FitProfile):
```yaml
spec:
  strategy: dsp
  parameters:
    enablePeakDetection: true
    enableNormalization: false
    method: "fft"  # or "maxValue"
    marginFraction: 0.2
    lowAmplitudeThreshold: 1.0
    highFrequencyThreshold: 0.05
    minNumOfSpectrumItems: 10
    maxNumOfSpectrumItems: 20
```

**How it works:**
1. **Preprocessing**: Imputes missing data, removes outliers, optionally normalizes
2. **Period Detection**: Uses FFT to find candidate periods, then ACF to verify
3. **Prediction**: Uses either:
   - **maxValue**: Takes max value at each time position across past cycles
   - **fft**: Filters frequency components and reconstructs signal
4. **Peak Detection**: Identifies anomalous peaks (optional)

**Parameters:**

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `method` | string | "fft" | Prediction method: "fft" or "maxValue" |
| `marginFraction` | float64 | 0.2 | Multiplier for predicted values (1 + marginFraction) |
| `lowAmplitudeThreshold` | float64 | 1.0 | Minimum spectral amplitude to keep |
| `highFrequencyThreshold` | float64 | 0.05 | Maximum frequency (Hz) to keep |
| `minNumOfSpectrumItems` | int | 10 | Minimum frequency components to retain |
| `maxNumOfSpectrumItems` | int | 20 | Maximum frequency components to retain |

**Best for:**
- Workloads with daily/weekly patterns
- Applications with predictable traffic cycles
- When you need to predict future usage, not just current

**Example:**
```yaml
apiVersion: autoscaling.sartorproj.io/v1alpha1
kind: FitProfile
metadata:
  name: dsp-web
  namespace: web
spec:
  strategy: dsp
  parameters:
    enablePeakDetection: true
    method: "fft"
    marginFraction: 0.15
    highFrequencyThreshold: 0.01  # Filter out cycles < 100 seconds
---
apiVersion: autoscaling.sartorproj.io/v1alpha1
kind: Tailoring
metadata:
  name: web-app
  namespace: web
spec:
  target:
    kind: Deployment
    name: web-app
  fitProfileRef:
    name: dsp-web
  writeBack:
    type: raw
    repository: "org/manifests"
    path: "apps/web-app/deployment.yaml"
```

### 3. ARIMA Strategy

The **arima** strategy forecasts usage with (seasonal) ARIMA models and recommends the upper bound of the prediction interval over a forecast horizon. It is fitted to the actual usage history, which the controller queries in addition to the percentiles. See [ARIMA Strategy Deep Dive](arima-strategy.md) for details.

**Configuration** (in a FitProfile, see `config/samples/fitprofile-arima.yaml`):
```yaml
spec:
  strategy: arima
  parameters:
    mode: auto          # or "fixed" with order / seasonalOrder / seasonalPeriod
    seasonal: true
    confidence: 0.95
    limitsConfidence: 0.99
    horizon: 24h
    maxSamples: 1000
    requestsBufferPercent: 20
    limitsBufferPercent: 30
    maxChangePercent: 50
```

**How it works:**
1. **History**: Queries CPU and memory usage over the analysis window at a 5m resolution
2. **Preparation**: Drops invalid samples, downsamples to at most `maxSamples` (keeping the maximum of each bucket) and interpolates gaps
3. **Model Selection**: Selects the ARIMA order automatically, including seasonal models with daily and weekly periods, or fits a fixed order
4. **Prediction**: Uses the upper bound of the `confidence` interval over the `horizon` for requests and of the `limitsConfidence` interval for limits
5. **Post-processing**: Applies buffers, `maxChangePercent`, safety rails and quotas exactly like the percentile strategy

If the history is missing, too short, constant or too sparse, or the model fit fails or returns an invalid forecast, the resource falls back to the percentile result. The reason is recorded in the recommendation metadata (e.g. `cpu_fallback`).

**Parameters:**

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `mode` | string | "auto" | "auto" (automatic order selection) or "fixed" |
| `seasonal` | bool | true | Consider daily and weekly seasonal models (auto mode) |
| `order` | [p, d, q] | | Non-seasonal order (fixed mode, required) |
| `seasonalOrder` | [P, D, Q] | [0, 0, 0] | Seasonal order (fixed mode) |
| `seasonalPeriod` | duration | | Seasonal period, e.g. "24h" (fixed mode) |
| `confidence` | float64 | 0.95 | Prediction interval level for requests |
| `limitsConfidence` | float64 | 0.99 | Prediction interval level for limits |
| `horizon` | duration | "24h" | Forecast horizon (at most 168h) |
| `maxSamples` | int | 1000 | Maximum samples a model is fitted to (100 to 5000) |
| `requestsBufferPercent` | float64 | 20 | Buffer added to requests |
| `limitsBufferPercent` | float64 | 30 | Buffer added to limits |
| `maxChangePercent` | float64 | 50 | Maximum change per recommendation |

**Best for:**
- Workloads with growing or shrinking usage
- Workloads with daily or weekly cycles (weekly needs an analysis window of at least 21 days)
- Sizing for a forecast horizon with a stated confidence

## Strategy Comparison

| Feature | Percentile | DSP | ARIMA |
|---------|-----------|-----|-------|
| **Speed** | Fast | Slower (requires FFT) | Slower (model fit, a few ms per resource) |
| **Period Detection** | No | Yes (daily/weekly) | Yes (daily/weekly) |
| **Trend** | No | No | Yes |
| **Prediction** | Current usage | Future usage | Future usage with a prediction interval |
| **Peak Detection** | No | Yes (optional) | No |
| **Normalization** | No | Yes (optional) | No |
| **Best for** | Stable workloads | Periodic workloads | Trending and periodic workloads |

## Creating Custom Strategies

You can create custom recommendation strategies by implementing the `strategy.Strategy` interface:

```go
type Strategy interface {
    Name() string
    Calculate(
        ctx context.Context,
        metrics []prometheus.ContainerMetrics,
        currentResources map[string]autoscalingv1alpha1.ResourceRequirements,
        config StrategyConfig,
    ) ([]Recommendation, error)
    ValidateConfig(config StrategyConfig) error
}
```

Then register it:

```go
calculator := recommender.NewCalculator()
calculator.RegisterStrategy(myCustomStrategy)
```

## Preprocessing Options

Both strategies support preprocessing (DSP strategy only):

- **Data Imputation**: Fills missing data points using linear interpolation
- **Outlier Removal**: Removes extreme values using percentile thresholds (P0.1, P99.9)
- **Normalization**: Z-score normalization for time series

## Peak Detection

When enabled, Sartor detects peaks in time series data:

- **Prominence**: Height of peak relative to surrounding valleys
- **Width**: Width at half prominence
- **Distance**: Minimum distance between peaks

Peaks are stored in recommendation metadata for analysis.

## References

- [Crane DSP Algorithm](https://gocrane.io/docs/core-concept/timeseries-forecasting-by-dsp/)
- [FFT Documentation](https://en.wikipedia.org/wiki/Fast_Fourier_transform)
- [Autocorrelation Function](https://en.wikipedia.org/wiki/Autocorrelation)
- [ARIMA Models](https://otexts.com/fpp3/arima.html)
