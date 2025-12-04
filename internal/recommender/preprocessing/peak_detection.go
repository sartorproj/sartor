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
)

// Peak represents a detected peak in a time series.
type Peak struct {
	Index int     // Index in the time series
	Value float64 // Value at the peak
	// Prominence is the height of the peak relative to surrounding valleys.
	Prominence float64
	// Width is the width of the peak at half prominence.
	Width float64
}

// PeakDetector detects peaks in time series data.
type PeakDetector struct {
	// MinProminence is the minimum prominence required for a peak.
	MinProminence float64
	// MinDistance is the minimum distance between peaks (in samples).
	MinDistance int
	// WindowSize is the window size for local peak detection.
	WindowSize int
}

// NewPeakDetector creates a new peak detector with default settings.
func NewPeakDetector() *PeakDetector {
	return &PeakDetector{
		MinProminence: 0.1,
		MinDistance:   5,
		WindowSize:    3,
	}
}

// DetectPeaks detects all peaks in the time series.
func (pd *PeakDetector) DetectPeaks(values []float64) []Peak {
	if len(values) < 3 {
		return nil
	}

	peaks := make([]Peak, 0)

	// Simple peak detection: local maxima
	for i := pd.WindowSize; i < len(values)-pd.WindowSize; i++ {
		if pd.isLocalMaximum(values, i) {
			prominence := pd.calculateProminence(values, i)
			if prominence >= pd.MinProminence {
				peaks = append(peaks, Peak{
					Index:      i,
					Value:      values[i],
					Prominence: prominence,
					Width:      pd.calculateWidth(values, i),
				})
			}
		}
	}

	// Filter by minimum distance
	return pd.filterByDistance(peaks)
}

// isLocalMaximum checks if the value at index i is a local maximum.
func (pd *PeakDetector) isLocalMaximum(values []float64, i int) bool {
	value := values[i]

	// Check left side
	for j := i - pd.WindowSize; j < i; j++ {
		if j >= 0 && values[j] >= value {
			return false
		}
	}

	// Check right side
	for j := i + 1; j <= i+pd.WindowSize; j++ {
		if j < len(values) && values[j] >= value {
			return false
		}
	}

	return true
}

// calculateProminence calculates the prominence of a peak.
// Prominence is the height of the peak relative to the higher of the two valleys on either side.
func (pd *PeakDetector) calculateProminence(values []float64, peakIndex int) float64 {
	peakValue := values[peakIndex]

	// Find the lowest point on the left side
	leftMin := peakValue
	for i := peakIndex - 1; i >= 0; i-- {
		if values[i] < leftMin {
			leftMin = values[i]
		}
		// Stop if we encounter a higher peak
		if values[i] > peakValue {
			break
		}
	}

	// Find the lowest point on the right side
	rightMin := peakValue
	for i := peakIndex + 1; i < len(values); i++ {
		if values[i] < rightMin {
			rightMin = values[i]
		}
		// Stop if we encounter a higher peak
		if values[i] > peakValue {
			break
		}
	}

	// Prominence is the height above the higher of the two valleys
	valleyHeight := math.Max(leftMin, rightMin)
	return peakValue - valleyHeight
}

// calculateWidth calculates the width of the peak at half prominence.
func (pd *PeakDetector) calculateWidth(values []float64, peakIndex int) float64 {
	peakValue := values[peakIndex]
	halfHeight := peakValue - pd.calculateProminence(values, peakIndex)/2

	// Find left edge
	leftEdge := peakIndex
	for i := peakIndex - 1; i >= 0; i-- {
		if values[i] < halfHeight {
			leftEdge = i
			break
		}
	}

	// Find right edge
	rightEdge := peakIndex
	for i := peakIndex + 1; i < len(values); i++ {
		if values[i] < halfHeight {
			rightEdge = i
			break
		}
	}

	return float64(rightEdge - leftEdge)
}

// filterByDistance filters peaks to ensure minimum distance between them.
func (pd *PeakDetector) filterByDistance(peaks []Peak) []Peak {
	if len(peaks) == 0 {
		return peaks
	}

	// Sort by prominence (descending)
	sorted := make([]Peak, len(peaks))
	copy(sorted, peaks)
	for i := 0; i < len(sorted)-1; i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[i].Prominence < sorted[j].Prominence {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	// Keep peaks that are far enough apart
	result := make([]Peak, 0)
	usedIndices := make(map[int]bool)

	for _, peak := range sorted {
		tooClose := false
		for usedIdx := range usedIndices {
			if abs(peak.Index-usedIdx) < pd.MinDistance {
				tooClose = true
				break
			}
		}

		if !tooClose {
			result = append(result, peak)
			usedIndices[peak.Index] = true
		}
	}

	return result
}

// DetectAnomalies detects anomalous peaks that deviate significantly from the mean.
func DetectAnomalies(values []float64, threshold float64) []int {
	if len(values) == 0 {
		return nil
	}

	// Calculate mean and standard deviation
	mean := 0.0
	for _, v := range values {
		mean += v
	}
	mean /= float64(len(values))

	variance := 0.0
	for _, v := range values {
		diff := v - mean
		variance += diff * diff
	}
	stdDev := math.Sqrt(variance / float64(len(values)))

	if stdDev == 0 {
		return nil
	}

	// Find anomalies (values beyond threshold * stdDev from mean)
	anomalies := make([]int, 0)
	for i, v := range values {
		zScore := math.Abs((v - mean) / stdDev)
		if zScore > threshold {
			anomalies = append(anomalies, i)
		}
	}

	return anomalies
}

// abs returns the absolute value of an integer.
func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
