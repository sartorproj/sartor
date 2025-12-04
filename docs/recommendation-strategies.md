# Recommendation Strategies

Sartor supports multiple recommendation strategies through a flexible plugin system. This document describes available strategies and how to configure them.

## Available Strategies

### 1. Percentile Strategy (Default)

The **percentile** strategy uses P95 for requests and P99 for limits, with intent-based buffers.

**Configuration:**
```yaml
spec:
  strategy: "percentile"  # or omit for default
  intent: Balanced
```

**How it works:**
- Calculates P95 and P99 from historical metrics
- Applies intent-based buffers (Eco: 10-20%, Balanced: 20-30%, Critical: 40-50%)
- Applies gradual change limits based on intent
- Respects safety rails and resource quotas

**Best for:**
- Most production workloads
- Workloads with stable usage patterns
- Quick recommendations without complex analysis

### 2. DSP Strategy

The **DSP (Digital Signal Processing)** strategy uses FFT and autocorrelation to detect periodic patterns and predict future usage.

**Configuration:**
```yaml
spec:
  strategy: "dsp"
  intent: Balanced
  enablePeakDetection: true
  enableNormalization: false
  strategyParameters:
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
kind: Tailoring
metadata:
  name: web-app
spec:
  target:
    kind: Deployment
    name: web-app
  intent: Balanced
  strategy: "dsp"
  enablePeakDetection: true
  strategyParameters:
    method: "fft"
    marginFraction: 0.15
    highFrequencyThreshold: 0.01  # Filter out cycles < 100 seconds
  writeBack:
    type: raw
    repository: "org/manifests"
    path: "apps/web-app/deployment.yaml"
```

## Strategy Comparison

| Feature | Percentile | DSP |
|---------|-----------|-----|
| **Speed** | Fast | Slower (requires FFT) |
| **Period Detection** | No | Yes (daily/weekly) |
| **Prediction** | Current usage | Future usage |
| **Peak Detection** | No | Yes (optional) |
| **Normalization** | No | Yes (optional) |
| **Best for** | Stable workloads | Periodic workloads |

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
