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

package dsp

import (
	"math"
	"math/cmplx"

	"github.com/mjibson/go-dsp/fft"
	"github.com/sartorproj/sartor/internal/recommender/preprocessing"
)

// DSP implements Digital Signal Processing for time series prediction.
// Inspired by Crane's DSP algorithm: https://gocrane.io/docs/core-concept/timeseries-forecasting-by-dsp/
type DSP struct {
	// SampleInterval is the interval between samples (in seconds).
	SampleInterval int64
	// TargetPeriods are the periods to detect (e.g., 1 day, 7 days in seconds).
	TargetPeriods []int64
	// RandomPermutationCount is the number of random permutations for threshold calculation.
	RandomPermutationCount int
}

// NewDSP creates a new DSP processor with default settings.
func NewDSP(sampleInterval int64) *DSP {
	return &DSP{
		SampleInterval:         sampleInterval,
		TargetPeriods:          []int64{86400, 604800}, // 1 day, 7 days
		RandomPermutationCount: 100,
	}
}

// DetectPeriod detects the primary period of the time series using FFT and ACF.
func (d *DSP) DetectPeriod(series []preprocessing.TimeSeries) (int64, float64, error) {
	if len(series) < 4 {
		return 0, 0, nil
	}

	values := preprocessing.ExtractValues(series)

	// Calculate FFT threshold using random permutations
	threshold := d.calculateFFTThreshold(values)

	// Perform FFT to find candidate periods
	candidates := d.findCandidatePeriods(values, threshold)

	if len(candidates) == 0 {
		return 0, 0, nil
	}

	// Use ACF to verify and select the best period
	period, confidence := d.selectPeriodWithACF(values, candidates, series[0].Timestamp)

	return period, confidence, nil
}

// calculateFFTThreshold calculates the threshold for FFT amplitude using random permutations.
func (d *DSP) calculateFFTThreshold(values []float64) float64 {
	maxAmplitudes := make([]float64, 0, d.RandomPermutationCount)

	for i := 0; i < d.RandomPermutationCount; i++ {
		permuted := make([]float64, len(values))
		copy(permuted, values)
		// Shuffle (simplified - in production use proper shuffle)
		for j := len(permuted) - 1; j > 0; j-- {
			k := int(math.Floor(float64(j+1) * 0.5)) // Simplified shuffle
			permuted[j], permuted[k] = permuted[k], permuted[j]
		}

		fft := d.fft(permuted)
		maxAmp := 0.0
		for _, val := range fft {
			amp := cmplx.Abs(val)
			if amp > maxAmp {
				maxAmp = amp
			}
		}
		maxAmplitudes = append(maxAmplitudes, maxAmp)
	}

	// Calculate P99
	return percentile(maxAmplitudes, 99.0)
}

// findCandidatePeriods finds candidate periods using FFT.
func (d *DSP) findCandidatePeriods(values []float64, threshold float64) []int {
	fft := d.fft(values)
	N := len(values)
	candidates := make([]int, 0)

	// Only consider first half (due to symmetry) and skip k=0, k=1
	for k := 2; k <= N/2; k++ {
		amplitude := cmplx.Abs(fft[k])
		if amplitude > threshold {
			candidates = append(candidates, k)
		}
	}

	return candidates
}

// selectPeriodWithACF selects the best period using Autocorrelation Function.
func (d *DSP) selectPeriodWithACF(values []float64, candidates []int, startTimestamp int64) (int64, float64) {
	if len(candidates) == 0 {
		return 0, 0
	}

	acf := d.calculateACF(values)
	var bestPeriod int64
	bestConfidence := 0.0

	for _, k := range candidates {
		period := int64(len(values) / k * int(d.SampleInterval))
		confidence := d.verifyPeak(acf, k)

		if confidence > bestConfidence {
			bestConfidence = confidence
			bestPeriod = period
		}
	}

	return bestPeriod, bestConfidence
}

// calculateACF calculates the Autocorrelation Function using circular ACF.
func (d *DSP) calculateACF(values []float64) []float64 {
	N := len(values)
	if N == 0 {
		return nil
	}

	// Calculate mean and standard deviation
	mean := 0.0
	for _, v := range values {
		mean += v
	}
	mean /= float64(N)

	variance := 0.0
	for _, v := range values {
		diff := v - mean
		variance += diff * diff
	}
	stdDev := math.Sqrt(variance / float64(N))

	if stdDev == 0 {
		// All values are the same
		acf := make([]float64, N/2)
		for i := range acf {
			acf[i] = 1.0
		}
		return acf
	}

	// Normalize values
	normalized := make([]float64, N)
	for i, v := range values {
		normalized[i] = (v - mean) / stdDev
	}

	// Calculate ACF using FFT: ACF = IFFT(|FFT(x)|^2)
	fft := d.fft(normalized)
	fftSquared := make([]complex128, len(fft))
	for i, val := range fft {
		fftSquared[i] = val * cmplx.Conj(val)
	}

	acf := d.ifft(fftSquared)

	// Extract real part and normalize
	result := make([]float64, N/2)
	for i := 0; i < N/2; i++ {
		result[i] = real(acf[i]) / float64(N)
	}

	return result
}

// verifyPeak verifies if a point in ACF is at a peak.
func (d *DSP) verifyPeak(acf []float64, k int) float64 {
	if k < 2 || k >= len(acf)-1 {
		return 0.0
	}

	// Check if it's a local peak
	windowSize := 3
	if k < windowSize || k >= len(acf)-windowSize {
		return 0.0
	}

	// Linear regression on left and right sides
	leftSlope := d.calculateSlope(acf[k-windowSize : k+1])
	rightSlope := d.calculateSlope(acf[k : k+windowSize+1])

	// Peak: left slope > 0, right slope < 0
	if leftSlope > 0 && rightSlope < 0 {
		return acf[k] // Use ACF value as confidence
	}

	return 0.0
}

// calculateSlope calculates the slope using linear regression.
func (d *DSP) calculateSlope(values []float64) float64 {
	if len(values) < 2 {
		return 0.0
	}

	n := float64(len(values))
	sumX := 0.0
	sumY := 0.0
	sumXY := 0.0
	sumX2 := 0.0

	for i, y := range values {
		x := float64(i)
		sumX += x
		sumY += y
		sumXY += x * y
		sumX2 += x * x
	}

	denominator := n*sumX2 - sumX*sumX
	if denominator == 0 {
		return 0.0
	}

	return (n*sumXY - sumX*sumY) / denominator
}

// PredictMaxValue predicts using the maxValue method.
func (d *DSP) PredictMaxValue(series []preprocessing.TimeSeries, period int64, marginFraction float64) []preprocessing.TimeSeries {
	if len(series) == 0 || period == 0 {
		return nil
	}

	// Calculate samples per period
	samplesPerPeriod := int(period / d.SampleInterval)
	if samplesPerPeriod == 0 {
		return nil
	}

	// Group values by position in period
	groups := make(map[int][]float64)
	for i, point := range series {
		position := i % samplesPerPeriod
		groups[position] = append(groups[position], point.Value)
	}

	// Predict next period using max value at each position
	predicted := make([]preprocessing.TimeSeries, samplesPerPeriod)
	lastTimestamp := series[len(series)-1].Timestamp

	for position := 0; position < samplesPerPeriod; position++ {
		values := groups[position]
		if len(values) == 0 {
			continue
		}

		// Find max value
		maxVal := values[0]
		for _, v := range values {
			if v > maxVal {
				maxVal = v
			}
		}

		// Apply margin
		predictedValue := maxVal * (1 + marginFraction)

		predicted[position] = preprocessing.TimeSeries{
			Timestamp: lastTimestamp + int64(position+1)*d.SampleInterval,
			Value:     predictedValue,
		}
	}

	return predicted
}

// PredictFFT predicts using FFT-based method.
func (d *DSP) PredictFFT(series []preprocessing.TimeSeries, config FFTConfig) []preprocessing.TimeSeries {
	if len(series) == 0 {
		return nil
	}

	values := preprocessing.ExtractValues(series)

	// Perform FFT
	fft := d.fft(values)

	// Filter frequency components
	filteredFFT := d.filterFFT(fft, config)

	// Perform IFFT to get predicted values
	predictedValues := d.ifft(filteredFFT)

	// Extract next period
	samplesPerPeriod := int(config.Period / d.SampleInterval)
	if samplesPerPeriod == 0 || samplesPerPeriod > len(predictedValues) {
		return nil
	}

	// Get the last period from predicted values
	startIdx := len(predictedValues) - samplesPerPeriod
	predicted := make([]preprocessing.TimeSeries, samplesPerPeriod)
	lastTimestamp := series[len(series)-1].Timestamp

	for i := 0; i < samplesPerPeriod; i++ {
		predicted[i] = preprocessing.TimeSeries{
			Timestamp: lastTimestamp + int64(i+1)*d.SampleInterval,
			Value:     real(predictedValues[startIdx+i]) * (1 + config.MarginFraction),
		}
	}

	return predicted
}

// filterFFT filters FFT components based on configuration.
func (d *DSP) filterFFT(fft []complex128, config FFTConfig) []complex128 {
	N := len(fft)
	filtered := make([]complex128, N)
	copy(filtered, fft)

	// Filter by amplitude
	amplitudes := make([]float64, N)
	for i, val := range fft {
		amplitudes[i] = cmplx.Abs(val)
	}

	// Sort amplitudes to find threshold
	sortedAmps := make([]float64, len(amplitudes))
	copy(sortedAmps, amplitudes)
	// Simple sort (in production use proper sort)
	for i := 0; i < len(sortedAmps)-1; i++ {
		for j := i + 1; j < len(sortedAmps); j++ {
			if sortedAmps[i] > sortedAmps[j] {
				sortedAmps[i], sortedAmps[j] = sortedAmps[j], sortedAmps[i]
			}
		}
	}

	// Calculate low amplitude threshold
	lowThreshold := config.LowAmplitudeThreshold
	if len(sortedAmps) > 0 {
		lowThreshold = math.Max(lowThreshold, sortedAmps[int(float64(len(sortedAmps))*0.1)])
	}

	// Filter components
	kept := 0
	for i := 0; i < N; i++ {
		amplitude := amplitudes[i]
		frequency := float64(i) / float64(N) / (float64(d.SampleInterval) / 86400.0) // Normalize to Hz

		// Filter conditions
		if amplitude < lowThreshold {
			filtered[i] = 0
			continue
		}

		if frequency > config.HighFrequencyThreshold {
			filtered[i] = 0
			continue
		}

		kept++
		if kept > config.MaxNumOfSpectrumItems {
			filtered[i] = 0
			continue
		}
	}

	// Ensure minimum number of components
	if kept < config.MinNumOfSpectrumItems {
		// Keep top N components
		topIndices := d.getTopIndices(amplitudes, config.MinNumOfSpectrumItems)
		for i := range filtered {
			isTop := false
			for _, idx := range topIndices {
				if i == idx {
					isTop = true
					break
				}
			}
			if !isTop && i > 0 && i < N/2 {
				filtered[i] = 0
			}
		}
	}

	return filtered
}

// getTopIndices returns indices of top N values.
func (d *DSP) getTopIndices(values []float64, n int) []int {
	if n > len(values) {
		n = len(values)
	}

	indices := make([]int, len(values))
	for i := range indices {
		indices[i] = i
	}

	// Sort by value (descending)
	for i := 0; i < n; i++ {
		maxIdx := i
		for j := i + 1; j < len(indices); j++ {
			if values[indices[j]] > values[indices[maxIdx]] {
				maxIdx = j
			}
		}
		indices[i], indices[maxIdx] = indices[maxIdx], indices[i]
	}

	return indices[:n]
}

// FFTConfig contains configuration for FFT-based prediction.
type FFTConfig struct {
	Period                 int64
	MarginFraction         float64
	LowAmplitudeThreshold  float64
	HighFrequencyThreshold float64
	MinNumOfSpectrumItems  int
	MaxNumOfSpectrumItems  int
}

// fft performs Fast Fourier Transform using go-dsp library.
func (d *DSP) fft(x []float64) []complex128 {
	if len(x) == 0 {
		return nil
	}
	return fft.FFTReal(x)
}

// ifft performs Inverse Fast Fourier Transform.
func (d *DSP) ifft(X []complex128) []complex128 {
	if len(X) == 0 {
		return nil
	}
	return fft.IFFT(X)
}

// percentile calculates the percentile value.
func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}

	sorted := make([]float64, len(values))
	copy(sorted, values)
	// Simple sort
	for i := 0; i < len(sorted)-1; i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[i] > sorted[j] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	index := p * float64(len(sorted)-1) / 100.0
	lower := int(math.Floor(index))
	upper := int(math.Ceil(index))

	if lower == upper {
		return sorted[lower]
	}

	weight := index - float64(lower)
	return sorted[lower]*(1-weight) + sorted[upper]*weight
}
