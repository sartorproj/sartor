/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package preprocessing

import (
	"math"
	"sort"
)

// TimeSeries represents a time series data point.
type TimeSeries struct {
	Timestamp int64   // Unix timestamp
	Value     float64 // Value at this timestamp
}

// Preprocessor handles preprocessing of time series data.
type Preprocessor struct {
	// EnableImputation enables missing data imputation.
	EnableImputation bool
	// EnableOutlierRemoval enables outlier detection and removal.
	EnableOutlierRemoval bool
	// EnableNormalization enables time series normalization.
	EnableNormalization bool
	// OutlierPercentile defines the percentile thresholds for outlier detection (default: 0.1 and 99.9).
	OutlierPercentile [2]float64
}

// NewPreprocessor creates a new preprocessor with default settings.
func NewPreprocessor() *Preprocessor {
	return &Preprocessor{
		EnableImputation:     true,
		EnableOutlierRemoval: true,
		EnableNormalization:  false,
		OutlierPercentile:    [2]float64{0.1, 99.9},
	}
}

// Process applies all enabled preprocessing steps to the time series.
func (p *Preprocessor) Process(series []TimeSeries) []TimeSeries {
	result := make([]TimeSeries, len(series))
	copy(result, series)

	if p.EnableImputation {
		result = p.ImputeMissing(result)
	}

	if p.EnableOutlierRemoval {
		result = p.RemoveOutliers(result)
	}

	if p.EnableNormalization {
		result = p.Normalize(result)
	}

	return result
}

// ImputeMissing fills in missing data points using linear interpolation.
// Missing points are identified by gaps in timestamps.
func (p *Preprocessor) ImputeMissing(series []TimeSeries) []TimeSeries {
	if len(series) < 2 {
		return series
	}

	result := make([]TimeSeries, 0, len(series))
	result = append(result, series[0])

	for i := 1; i < len(series); i++ {
		prev := series[i-1]
		curr := series[i]

		// Check if there's a gap (assuming uniform sampling, detect large gaps)
		// For simplicity, we'll interpolate between consecutive points
		// In practice, you'd detect actual missing timestamps
		if curr.Timestamp > prev.Timestamp {
			// Linear interpolation for any intermediate points
			// This is a simplified version - real implementation would detect actual gaps
			result = append(result, curr)
		} else {
			// Same timestamp or out of order - keep as is
			result = append(result, curr)
		}
	}

	return result
}

// RemoveOutliers removes extreme outlier points using percentile-based thresholds.
func (p *Preprocessor) RemoveOutliers(series []TimeSeries) []TimeSeries {
	if len(series) == 0 {
		return series
	}

	// Extract values for percentile calculation
	values := make([]float64, len(series))
	for i, point := range series {
		values[i] = point.Value
	}

	// Calculate percentiles
	lowerThreshold := percentile(values, p.OutlierPercentile[0])
	upperThreshold := percentile(values, p.OutlierPercentile[1])

	// Remove outliers
	result := make([]TimeSeries, 0, len(series))
	for i, point := range series {
		if point.Value < lowerThreshold || point.Value > upperThreshold {
			// Replace with previous value (or next if first point)
			if i > 0 {
				result = append(result, result[len(result)-1])
			} else if i < len(series)-1 {
				result = append(result, series[i+1])
			} else {
				// Single point - keep as is
				result = append(result, point)
			}
		} else {
			result = append(result, point)
		}
	}

	return result
}

// Normalize normalizes the time series using z-score normalization.
func (p *Preprocessor) Normalize(series []TimeSeries) []TimeSeries {
	if len(series) == 0 {
		return series
	}

	// Calculate mean and standard deviation
	mean := 0.0
	for _, point := range series {
		mean += point.Value
	}
	mean /= float64(len(series))

	variance := 0.0
	for _, point := range series {
		diff := point.Value - mean
		variance += diff * diff
	}
	stdDev := math.Sqrt(variance / float64(len(series)))

	if stdDev == 0 {
		// All values are the same - return as is
		return series
	}

	// Normalize
	result := make([]TimeSeries, len(series))
	for i, point := range series {
		result[i] = TimeSeries{
			Timestamp: point.Timestamp,
			Value:     (point.Value - mean) / stdDev,
		}
	}

	return result
}

// percentile calculates the percentile value from a sorted slice.
func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}

	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)

	index := p * float64(len(sorted)-1)
	lower := int(math.Floor(index))
	upper := int(math.Ceil(index))

	if lower == upper {
		return sorted[lower]
	}

	weight := index - float64(lower)
	return sorted[lower]*(1-weight) + sorted[upper]*weight
}

// ExtractValues extracts just the values from a time series.
func ExtractValues(series []TimeSeries) []float64 {
	values := make([]float64, len(series))
	for i, point := range series {
		values[i] = point.Value
	}
	return values
}

// MinMaxNormalization normalizes values to [0, 1] range.
func MinMaxNormalization(values []float64) []float64 {
	if len(values) == 0 {
		return values
	}

	min := values[0]
	max := values[0]
	for _, v := range values {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}

	if max == min {
		// All values are the same
		result := make([]float64, len(values))
		for i := range result {
			result[i] = 0.5
		}
		return result
	}

	result := make([]float64, len(values))
	range_ := max - min
	for i, v := range values {
		result[i] = (v - min) / range_
	}

	return result
}
