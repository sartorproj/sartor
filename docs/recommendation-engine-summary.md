# Recommendation Engine Refactoring Summary

## Overview

The recommendation engine has been refactored into a flexible, plugin-based architecture supporting multiple recommendation strategies, including advanced DSP-based prediction inspired by [Crane](https://gocrane.io/docs/core-concept/timeseries-forecasting-by-dsp/).

## Architecture

### Plugin System

The recommendation engine now uses a plugin-based architecture:

```
Calculator
  └── StrategyRegistry
      ├── PercentileStrategy (default)
      ├── DSPStrategy
      └── [Custom Strategies]
```

### Strategy Interface

All strategies implement the `strategy.Strategy` interface:

```go
type Strategy interface {
    Name() string
    Calculate(ctx, metrics, currentResources, config) ([]Recommendation, error)
    ValidateConfig(config StrategyConfig) error
}
```

## Implemented Strategies

### 1. Percentile Strategy (Default)

**Location**: `internal/recommender/strategy/percentile.go`

- Uses P95 for requests, P99 for limits
- Applies intent-based buffers (Eco: 10-20%, Balanced: 20-30%, Critical: 40-50%)
- Fast and reliable for most workloads
- Backward compatible with existing behavior

### 2. DSP Strategy

**Location**: `internal/recommender/strategy/dsp.go`

**Features:**
- **Period Detection**: Uses FFT and ACF to detect daily/weekly patterns
- **Two Prediction Methods**:
  - `maxValue`: Takes max value at each time position across cycles
  - `fft`: Filters frequency components and reconstructs signal
- **Preprocessing**: Data imputation, outlier removal, normalization
- **Peak Detection**: Identifies anomalous peaks in time series

**DSP Processor**: `internal/recommender/dsp/dsp.go`
- FFT-based frequency analysis
- Autocorrelation Function (ACF) for period verification
- Random permutation threshold calculation
- Peak verification using linear regression

## Preprocessing

**Location**: `internal/recommender/preprocessing/`

### Preprocessor (`preprocessor.go`)
- **Data Imputation**: Linear interpolation for missing points
- **Outlier Removal**: Percentile-based filtering (P0.1, P99.9)
- **Normalization**: Z-score normalization (optional)
- **Min-Max Normalization**: Alternative normalization method

### Peak Detection (`peak_detection.go`)
- **Local Maxima Detection**: Window-based peak finding
- **Prominence Calculation**: Height relative to valleys
- **Width Calculation**: Width at half prominence
- **Distance Filtering**: Ensures minimum distance between peaks
- **Anomaly Detection**: Z-score based outlier identification

## Configuration

### Tailoring CRD Updates

New fields added to `TailoringSpec`:

```yaml
spec:
  strategy: "dsp"  # or "percentile" (default)
  enablePeakDetection: true
  enableNormalization: false
  strategyParameters:
    method: "fft"
    marginFraction: 0.2
    lowAmplitudeThreshold: 1.0
    highFrequencyThreshold: 0.05
    minNumOfSpectrumItems: 10
    maxNumOfSpectrumItems: 20
```

**Note**: `strategyParameters` uses `runtime.RawExtension` for CRD compatibility.

## Usage Examples

### Percentile Strategy (Default)

```yaml
apiVersion: autoscaling.sartorproj.io/v1alpha1
kind: Tailoring
spec:
  strategy: "percentile"  # or omit
  intent: Balanced
```

### DSP Strategy with FFT

```yaml
apiVersion: autoscaling.sartorproj.io/v1alpha1
kind: Tailoring
spec:
  strategy: "dsp"
  intent: Balanced
  enablePeakDetection: true
  strategyParameters:
    method: "fft"
    marginFraction: 0.15
    highFrequencyThreshold: 0.01
```

### DSP Strategy with maxValue

```yaml
apiVersion: autoscaling.sartorproj.io/v1alpha1
kind: Tailoring
spec:
  strategy: "dsp"
  strategyParameters:
    method: "maxValue"
    marginFraction: 0.2
```

## Implementation Details

### Calculator Refactoring

**Old API** (deprecated but still supported):
```go
calculator := NewCalculator(safetyRails, quota)
recommendations := calculator.Calculate(metrics, currentResources, intent)
```

**New API**:
```go
calculator := NewCalculator()
recommendations, err := calculator.Calculate(
    ctx, metrics, currentResources, intent,
    safetyRails, quota, analysisWindow,
    strategyName, strategyParams,
    enablePeakDetection, enableNormalization,
)
```

### Strategy Registry

Strategies are automatically registered:
- `PercentileStrategy` - Always registered
- `DSPStrategy` - Always registered
- Custom strategies can be registered via `calculator.RegisterStrategy()`

### Backward Compatibility

The old `Recommendation` type is maintained for compatibility:
- `strategy.Recommendation` - New format with confidence, method, metadata
- `recommender.Recommendation` - Legacy format (deprecated)

Conversion functions handle the mapping between formats.

## Performance Considerations

### Percentile Strategy
- **Time Complexity**: O(N) where N is number of containers
- **Memory**: Minimal
- **Best for**: Real-time recommendations

### DSP Strategy
- **Time Complexity**: O(N log N) for FFT, where N is number of samples
- **Memory**: Stores full time series
- **Best for**: Batch analysis, periodic workloads

## Testing

Unit tests added for:
- Strategy interface and registry
- Percentile strategy calculations
- DSP period detection
- FFT and ACF calculations
- Preprocessing functions
- Peak detection algorithms

## Future Enhancements

Potential additions:
- **Machine Learning Strategy**: Use ML models for prediction
- **Hybrid Strategy**: Combine multiple strategies
- **Adaptive Strategy Selection**: Auto-select best strategy based on workload patterns
- **Real-time Time Series**: Fetch actual historical data from Prometheus (currently uses synthetic data)

## References

- [Crane DSP Algorithm](https://gocrane.io/docs/core-concept/timeseries-forecasting-by-dsp/)
- [FFT Algorithm](https://en.wikipedia.org/wiki/Fast_Fourier_transform)
- [Autocorrelation Function](https://en.wikipedia.org/wiki/Autocorrelation)
