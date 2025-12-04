# DSP Strategy Deep Dive

The DSP (Digital Signal Processing) strategy uses advanced signal processing techniques to detect periodic patterns in resource usage and predict future requirements.

## How It Works

### 1. Data Preprocessing

Before analysis, the time series data undergoes preprocessing:

**Data Imputation**
- Fills missing data points using linear interpolation
- Ensures continuous time series for FFT analysis

**Outlier Removal**
- Removes extreme values using percentile thresholds (P0.1, P99.9)
- Prevents outliers from interfering with period detection
- Replaces outliers with previous valid value

**Normalization** (Optional)
- Z-score normalization: `(value - mean) / stdDev`
- Useful for comparing different resource types
- Can be enabled via `enableNormalization: true`

### 2. Period Detection

The DSP algorithm detects periodic patterns using two methods:

#### Fast Fourier Transform (FFT)

FFT transforms time domain signals into frequency domain:

1. **Calculate FFT Threshold**: Uses random permutations to determine significance threshold
2. **Find Candidate Periods**: Identifies frequency components above threshold
3. **Filter Candidates**: Only considers periods that can be reliably detected

**Example:**
```
Time Series: [100m, 150m, 200m, 150m, 100m, ...] (daily pattern)
FFT Output:  High amplitude at k corresponding to 1-day period
```

#### Autocorrelation Function (ACF)

ACF verifies periodicity by measuring self-similarity:

1. **Calculate Circular ACF**: Extends time series periodically
2. **Find Peaks**: Identifies peaks in ACF at candidate periods
3. **Select Best Period**: Chooses period with highest confidence

**Peak Detection:**
- Uses linear regression on left/right sides
- Peak: left slope > 0, right slope < 0
- Confidence based on ACF value at peak

### 3. Prediction Methods

Once a period is detected, two prediction methods are available:

#### maxValue Method

For each time position in the period:
1. Collect values from all past cycles at that position
2. Take the maximum value
3. Apply margin fraction: `predicted = max * (1 + marginFraction)`

**Best for:**
- Workloads with consistent daily patterns
- When you want to be conservative (use max)

**Example:**
```
Past cycles at 6 PM:
  Day 1: 200m
  Day 2: 180m
  Day 3: 220m
  Day 4: 190m

Predicted (marginFraction=0.1): 220m * 1.1 = 242m
```

#### fft Method

1. Perform FFT on historical data
2. Filter frequency components:
   - Remove low amplitude (noise)
   - Remove high frequency (short cycles)
   - Keep top N components
3. Perform IFFT to reconstruct signal
4. Extract next period from reconstructed signal
5. Apply margin fraction

**Best for:**
- Workloads with complex patterns
- When you want smoother predictions
- Captures multiple periodic components

**Filtering Parameters:**
- `lowAmplitudeThreshold`: Minimum spectral amplitude (default: 1.0)
- `highFrequencyThreshold`: Maximum frequency in Hz (default: 0.05)
- `minNumOfSpectrumItems`: Minimum components to keep (default: 10)
- `maxNumOfSpectrumItems`: Maximum components to keep (default: 20)

### 4. Peak Detection (Optional)

When `enablePeakDetection: true`, Sartor identifies peaks in the time series:

- **Prominence**: Height relative to surrounding valleys
- **Width**: Width at half prominence
- **Distance**: Minimum distance between peaks

Peaks are stored in recommendation metadata for analysis.

## Configuration Examples

### Basic DSP with FFT

```yaml
spec:
  strategy: "dsp"
  strategyParameters:
    method: "fft"
    marginFraction: 0.2
    lowAmplitudeThreshold: 1.0
    highFrequencyThreshold: 0.05
```

### DSP with maxValue Method

```yaml
spec:
  strategy: "dsp"
  strategyParameters:
    method: "maxValue"
    marginFraction: 0.15
```

### DSP with Peak Detection

```yaml
spec:
  strategy: "dsp"
  enablePeakDetection: true
  strategyParameters:
    method: "fft"
    marginFraction: 0.2
```

### DSP with Normalization

```yaml
spec:
  strategy: "dsp"
  enableNormalization: true
  enableOutlierRemoval: true
  strategyParameters:
    method: "fft"
```

## When to Use DSP Strategy

**Use DSP when:**
- Workloads have daily/weekly patterns
- You need to predict future usage (not just current)
- Traffic patterns are predictable
- You want to account for periodic spikes

**Use Percentile when:**
- Usage is relatively stable
- You need fast recommendations
- Patterns are not clearly periodic
- Simple P95/P99 is sufficient

## Performance Considerations

- **FFT Complexity**: O(N log N) where N is number of samples
- **ACF Calculation**: Uses FFT, also O(N log N)
- **Memory**: Stores full time series in memory
- **CPU**: More CPU-intensive than percentile strategy

For large time series (>10k points), consider:
- Reducing `historyLength` in Prometheus queries
- Using percentile strategy for real-time recommendations
- Using DSP for scheduled batch analysis

## References

- [Crane DSP Documentation](https://gocrane.io/docs/core-concept/timeseries-forecasting-by-dsp/)
- [FFT Algorithm](https://en.wikipedia.org/wiki/Fast_Fourier_transform)
- [Autocorrelation](https://en.wikipedia.org/wiki/Autocorrelation)
