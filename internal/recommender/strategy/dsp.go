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

package strategy

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/metrics/prometheus"
	"github.com/sartorproj/sartor/internal/recommender/dsp"
	"github.com/sartorproj/sartor/internal/recommender/preprocessing"
)

const (
	// methodFFT is the FFT prediction method.
	methodFFT = "fft"
	// methodMaxValue is the max value prediction method.
	methodMaxValue = "maxValue"
	// resourceTypeCPU is the CPU resource type.
	resourceTypeCPU = "cpu"
	// resourceTypeMemory is the memory resource type.
	resourceTypeMemory = "memory"
)

// DSPStrategy implements DSP-based time series prediction for recommendations.
// Inspired by Crane's DSP algorithm: https://gocrane.io/docs/core-concept/timeseries-forecasting-by-dsp/
type DSPStrategy struct {
	dspProcessor *dsp.DSP
	preprocessor *preprocessing.Preprocessor
	peakDetector *preprocessing.PeakDetector
}

// NewDSPStrategy creates a new DSP-based strategy.
func NewDSPStrategy(sampleInterval time.Duration) *DSPStrategy {
	return &DSPStrategy{
		dspProcessor: dsp.NewDSP(int64(sampleInterval.Seconds())),
		preprocessor: preprocessing.NewPreprocessor(),
		peakDetector: preprocessing.NewPeakDetector(),
	}
}

// Name returns the strategy name.
func (s *DSPStrategy) Name() string {
	return "dsp"
}

// Calculate computes recommendations using DSP-based time series prediction.
func (s *DSPStrategy) Calculate(
	ctx context.Context,
	metrics []prometheus.ContainerMetrics,
	currentResources map[string]autoscalingv1alpha1.ResourceRequirements,
	config StrategyConfig,
) ([]Recommendation, error) {
	recommendations := make([]Recommendation, 0, len(metrics))

	// Enable preprocessing based on config
	s.preprocessor.EnableNormalization = config.EnableNormalization
	s.preprocessor.EnableOutlierRemoval = config.EnableOutlierRemoval

	for _, m := range metrics {
		rec, err := s.calculateForContainer(ctx, m, currentResources[m.ContainerName], config)
		if err != nil {
			return nil, fmt.Errorf("failed to calculate for container %s: %w", m.ContainerName, err)
		}
		recommendations = append(recommendations, rec)
	}

	return recommendations, nil
}

// calculateForContainer computes recommendation for a single container using DSP.
func (s *DSPStrategy) calculateForContainer(
	_ context.Context,
	metrics prometheus.ContainerMetrics,
	_ autoscalingv1alpha1.ResourceRequirements,
	config StrategyConfig,
) (Recommendation, error) {
	rec := Recommendation{
		ContainerName: metrics.ContainerName,
		Method:        "dsp",
		Metadata:      make(map[string]interface{}),
	}

	// Get time series data from metrics
	// Note: This assumes metrics contain time series data
	// In practice, you'd fetch historical time series from Prometheus
	cpuSeries, memSeries, err := s.extractTimeSeries(metrics)
	if err != nil {
		return rec, err
	}

	// Process time series
	if config.EnableNormalization || config.EnableOutlierRemoval {
		cpuSeries = s.preprocessor.Process(cpuSeries)
		memSeries = s.preprocessor.Process(memSeries)
	}

	// Detect peaks if enabled
	if config.EnablePeakDetection {
		cpuPeaks := s.peakDetector.DetectPeaks(preprocessing.ExtractValues(cpuSeries))
		memPeaks := s.peakDetector.DetectPeaks(preprocessing.ExtractValues(memSeries))
		rec.Metadata["cpu_peaks"] = len(cpuPeaks)
		rec.Metadata["memory_peaks"] = len(memPeaks)
	}

	// Get DSP parameters from config
	dspParams := s.getDSPParameters(config)

	// Predict CPU using DSP
	if len(cpuSeries) > 0 {
		cpuPrediction, confidence, err := s.predictResource(cpuSeries, dspParams, resourceTypeCPU, config)
		if err == nil && cpuPrediction != nil {
			// Use predicted values for requests and limits
			rec.Requests.CPU = cpuPrediction.Requests
			rec.Limits.CPU = cpuPrediction.Limits
			rec.Confidence = confidence
			rec.Metadata["cpu_period"] = dspParams.Period
		} else {
			// Fallback to P95/P99 if DSP fails
			if metrics.P95CPU != nil {
				rec.Requests.CPU = metrics.P95CPU
			}
			if metrics.P99CPU != nil {
				rec.Limits.CPU = metrics.P99CPU
			}
		}
	}

	// Predict Memory using DSP
	if len(memSeries) > 0 {
		memPrediction, confidence, err := s.predictResource(memSeries, dspParams, resourceTypeMemory, config)
		if err == nil && memPrediction != nil {
			rec.Requests.Memory = memPrediction.Requests
			rec.Limits.Memory = memPrediction.Limits
			if confidence < rec.Confidence {
				rec.Confidence = confidence
			}
			rec.Metadata["memory_period"] = dspParams.Period
		} else {
			// Fallback to P95/P99 if DSP fails
			if metrics.P95Memory != nil {
				rec.Requests.Memory = metrics.P95Memory
			}
			if metrics.P99Memory != nil {
				rec.Limits.Memory = metrics.P99Memory
			}
		}
	}

	// Apply safety rails
	s.applySafetyRails(&rec, config.SafetyRails)

	// Apply quota limits
	if config.ResourceQuota != nil {
		s.applyQuotaLimits(&rec, config.ResourceQuota)
	}

	return rec, nil
}

// extractTimeSeries extracts time series from metrics.
// In practice, this would fetch historical data from Prometheus.
// nolint:unparam // Error return is for future expansion when fetching real historical data.
func (s *DSPStrategy) extractTimeSeries(metrics prometheus.ContainerMetrics) ([]preprocessing.TimeSeries, []preprocessing.TimeSeries, error) {
	// For now, create synthetic time series from P95/P99 values
	// In production, fetch actual historical time series from Prometheus
	now := time.Now().Unix()
	sampleInterval := s.dspProcessor.SampleInterval

	cpuSeries := make([]preprocessing.TimeSeries, 0)
	memSeries := make([]preprocessing.TimeSeries, 0)

	// Create a week of data points (assuming daily periodicity)
	for i := 0; i < 7*24; i++ {
		timestamp := now - int64((7*24-i)*int(sampleInterval))

		// Use P95/P99 as base values with some variation
		if metrics.P95CPU != nil {
			cpuValue := metrics.P95CPU.AsApproximateFloat64()
			cpuSeries = append(cpuSeries, preprocessing.TimeSeries{
				Timestamp: timestamp,
				Value:     cpuValue,
			})
		}

		if metrics.P95Memory != nil {
			memValue := metrics.P95Memory.AsApproximateFloat64()
			memSeries = append(memSeries, preprocessing.TimeSeries{
				Timestamp: timestamp,
				Value:     memValue,
			})
		}
	}

	return cpuSeries, memSeries, nil
}

// predictResource predicts resource usage using DSP.
func (s *DSPStrategy) predictResource(
	series []preprocessing.TimeSeries,
	params dsp.FFTConfig,
	resourceType string,
	config StrategyConfig,
) (*ResourcePrediction, float64, error) {
	if len(series) == 0 {
		return nil, 0, fmt.Errorf("empty time series")
	}

	// Detect period
	period, confidence, err := s.dspProcessor.DetectPeriod(series)
	if err != nil {
		return nil, 0, err
	}

	if period == 0 {
		// No period detected, use default
		period = 86400 // 1 day
		confidence = 0.5
	}

	params.Period = period

	// Choose prediction method based on config
	method := methodFFT
	if config.Parameters != nil {
		if m, ok := config.Parameters["method"].(string); ok {
			method = m
		}
	}

	var predicted []preprocessing.TimeSeries

	switch method {
	case methodMaxValue:
		marginFraction := params.MarginFraction
		predicted = s.dspProcessor.PredictMaxValue(series, period, marginFraction)
	case methodFFT:
		fallthrough
	default:
		predicted = s.dspProcessor.PredictFFT(series, params)
	}

	if len(predicted) == 0 {
		return nil, 0, fmt.Errorf("prediction returned empty result")
	}

	// Find max values in predicted series for limits
	maxValue := predicted[0].Value
	p95Value := predicted[0].Value
	for _, point := range predicted {
		if point.Value > maxValue {
			maxValue = point.Value
		}
	}

	// Calculate P95 of predicted values
	values := preprocessing.ExtractValues(predicted)
	p95Idx := int(float64(len(values)) * 0.95)
	if p95Idx < len(values) {
		p95Value = values[p95Idx]
	}

	// Convert to resource.Quantity
	var requests, limits *resource.Quantity
	if resourceType == resourceTypeCPU {
		requests = resource.NewMilliQuantity(int64(p95Value*1000), resource.DecimalSI)
		limits = resource.NewMilliQuantity(int64(maxValue*1000), resource.DecimalSI)
	} else {
		requests = resource.NewQuantity(int64(p95Value), resource.BinarySI)
		limits = resource.NewQuantity(int64(maxValue), resource.BinarySI)
	}

	return &ResourcePrediction{
		Requests: requests,
		Limits:   limits,
	}, confidence, nil
}

// ResourcePrediction holds predicted resource values.
type ResourcePrediction struct {
	Requests *resource.Quantity
	Limits   *resource.Quantity
}

// getDSPParameters extracts DSP parameters from config.
func (s *DSPStrategy) getDSPParameters(config StrategyConfig) dsp.FFTConfig {
	params := dsp.FFTConfig{
		Period:                 86400, // Default 1 day
		MarginFraction:         0.2,
		LowAmplitudeThreshold:  1.0,
		HighFrequencyThreshold: 0.05,
		MinNumOfSpectrumItems:  10,
		MaxNumOfSpectrumItems:  20,
	}

	if config.Parameters != nil {
		if period, ok := config.Parameters["period"].(int64); ok {
			params.Period = period
		}
		if mf, ok := config.Parameters["marginFraction"].(float64); ok {
			params.MarginFraction = mf
		}
		if lat, ok := config.Parameters["lowAmplitudeThreshold"].(float64); ok {
			params.LowAmplitudeThreshold = lat
		}
		if hft, ok := config.Parameters["highFrequencyThreshold"].(float64); ok {
			params.HighFrequencyThreshold = hft
		}
		if min, ok := config.Parameters["minNumOfSpectrumItems"].(int); ok {
			params.MinNumOfSpectrumItems = min
		}
		if max, ok := config.Parameters["maxNumOfSpectrumItems"].(int); ok {
			params.MaxNumOfSpectrumItems = max
		}
	}

	return params
}

// applySafetyRails applies safety rails to recommendations.
func (s *DSPStrategy) applySafetyRails(rec *Recommendation, rails SafetyRailsConfig) {
	if rails.MinCPU != nil && rec.Requests.CPU != nil && rec.Requests.CPU.Cmp(*rails.MinCPU) < 0 {
		cpuCopy := rails.MinCPU.DeepCopy()
		rec.Requests.CPU = &cpuCopy
		rec.Capped = true
		rec.CappedReason = "capped at minimum CPU safety rail"
	}
	if rails.MaxCPU != nil && rec.Limits.CPU != nil && rec.Limits.CPU.Cmp(*rails.MaxCPU) > 0 {
		cpuCopy := rails.MaxCPU.DeepCopy()
		rec.Limits.CPU = &cpuCopy
		rec.Capped = true
		rec.CappedReason = "capped at maximum CPU safety rail"
	}
	if rails.MinMemory != nil && rec.Requests.Memory != nil && rec.Requests.Memory.Cmp(*rails.MinMemory) < 0 {
		memCopy := rails.MinMemory.DeepCopy()
		rec.Requests.Memory = &memCopy
		rec.Capped = true
		rec.CappedReason = "capped at minimum memory safety rail"
	}
	if rails.MaxMemory != nil && rec.Limits.Memory != nil && rec.Limits.Memory.Cmp(*rails.MaxMemory) > 0 {
		memCopy := rails.MaxMemory.DeepCopy()
		rec.Limits.Memory = &memCopy
		rec.Capped = true
		rec.CappedReason = "capped at maximum memory safety rail"
	}
}

// applyQuotaLimits applies quota limits to recommendations.
func (s *DSPStrategy) applyQuotaLimits(rec *Recommendation, quota *ResourceQuotaConfig) {
	if quota == nil {
		return
	}

	if quota.CPULimit != nil {
		if rec.Requests.CPU != nil && rec.Requests.CPU.Cmp(*quota.CPULimit) > 0 {
			cpuCopy := quota.CPULimit.DeepCopy()
			rec.Requests.CPU = &cpuCopy
			rec.Capped = true
			rec.CappedReason = "capped at ResourceQuota CPU limit"
		}
		if rec.Limits.CPU != nil && rec.Limits.CPU.Cmp(*quota.CPULimit) > 0 {
			cpuCopy := quota.CPULimit.DeepCopy()
			rec.Limits.CPU = &cpuCopy
			rec.Capped = true
			rec.CappedReason = "capped at ResourceQuota CPU limit"
		}
	}

	if quota.MemoryLimit != nil {
		if rec.Requests.Memory != nil && rec.Requests.Memory.Cmp(*quota.MemoryLimit) > 0 {
			memCopy := quota.MemoryLimit.DeepCopy()
			rec.Requests.Memory = &memCopy
			rec.Capped = true
			rec.CappedReason = "capped at ResourceQuota memory limit"
		}
		if rec.Limits.Memory != nil && rec.Limits.Memory.Cmp(*quota.MemoryLimit) > 0 {
			memCopy := quota.MemoryLimit.DeepCopy()
			rec.Limits.Memory = &memCopy
			rec.Capped = true
			rec.CappedReason = "capped at ResourceQuota memory limit"
		}
	}
}

// ValidateConfig validates the DSP strategy configuration.
func (s *DSPStrategy) ValidateConfig(config StrategyConfig) error {
	// Intent is optional now (can use FitProfile instead)
	return nil
}

// ValidateFitProfileParameters validates FitProfile parameters for DSP strategy.
func (s *DSPStrategy) ValidateFitProfileParameters(parameters map[string]interface{}) error {
	if parameters == nil {
		return nil // Parameters are optional
	}

	// Validate method
	if method, ok := parameters["method"].(string); ok {
		if method != methodFFT && method != methodMaxValue {
			return fmt.Errorf("method must be '%s' or '%s', got %s", methodFFT, methodMaxValue, method)
		}
	}

	// Validate marginFraction
	if mf, ok := parameters["marginFraction"].(float64); ok {
		if mf < 0 || mf > 2.0 {
			return fmt.Errorf("marginFraction must be between 0 and 2.0, got %f", mf)
		}
	}

	// Validate lowAmplitudeThreshold
	if lat, ok := parameters["lowAmplitudeThreshold"].(float64); ok {
		if lat < 0 {
			return fmt.Errorf("lowAmplitudeThreshold must be >= 0, got %f", lat)
		}
	}

	// Validate highFrequencyThreshold
	if hft, ok := parameters["highFrequencyThreshold"].(float64); ok {
		if hft < 0 || hft > 1.0 {
			return fmt.Errorf("highFrequencyThreshold must be between 0 and 1.0, got %f", hft)
		}
	}

	// Validate minNumOfSpectrumItems
	if min, ok := parameters["minNumOfSpectrumItems"].(float64); ok {
		if min < 0 || min > 100 {
			return fmt.Errorf("minNumOfSpectrumItems must be between 0 and 100, got %f", min)
		}
	}

	// Validate maxNumOfSpectrumItems
	if max, ok := parameters["maxNumOfSpectrumItems"].(float64); ok {
		if max < 0 || max > 100 {
			return fmt.Errorf("maxNumOfSpectrumItems must be between 0 and 100, got %f", max)
		}
	}

	// Validate min/max relationship
	if min, ok := parameters["minNumOfSpectrumItems"].(float64); ok {
		if max, ok2 := parameters["maxNumOfSpectrumItems"].(float64); ok2 {
			if min > max {
				return fmt.Errorf("minNumOfSpectrumItems (%f) must be <= maxNumOfSpectrumItems (%f)", min, max)
			}
		}
	}

	return nil
}

// GetDefaultFitProfileSpec returns the default FitProfile spec for DSP strategy.
func (s *DSPStrategy) GetDefaultFitProfileSpec() FitProfileSpec {
	exampleParams := map[string]interface{}{
		"method":                 "fft",
		"marginFraction":         0.2,
		"lowAmplitudeThreshold":  1.0,
		"highFrequencyThreshold": 0.05,
		"minNumOfSpectrumItems":  10,
		"maxNumOfSpectrumItems":  20,
	}

	var paramsSchema map[string]interface{}
	_ = json.Unmarshal([]byte(`{
		"type": "object",
		"properties": {
			"method": {
				"type": "string",
				"enum": ["fft", "maxValue"],
				"description": "Prediction method: 'fft' for frequency filtering, 'maxValue' for max across cycles",
				"default": "fft"
			},
			"marginFraction": {
				"type": "number",
				"description": "Multiplier for predicted values (1 + marginFraction)",
				"minimum": 0,
				"maximum": 2.0,
				"default": 0.2
			},
			"lowAmplitudeThreshold": {
				"type": "number",
				"description": "Minimum spectral amplitude to keep",
				"minimum": 0,
				"default": 1.0
			},
			"highFrequencyThreshold": {
				"type": "number",
				"description": "Maximum frequency (Hz) to keep",
				"minimum": 0,
				"maximum": 1.0,
				"default": 0.05
			},
			"minNumOfSpectrumItems": {
				"type": "integer",
				"description": "Minimum frequency components to retain",
				"minimum": 0,
				"maximum": 100,
				"default": 10
			},
			"maxNumOfSpectrumItems": {
				"type": "integer",
				"description": "Maximum frequency components to retain",
				"minimum": 0,
				"maximum": 100,
				"default": 20
			}
		}
	}`), &paramsSchema)

	return FitProfileSpec{
		Strategy:          "dsp",
		DisplayName:       "DSP-based Time Series Prediction",
		Description:       "Uses FFT and autocorrelation to detect periodic patterns and predict future usage",
		ParametersSchema:  paramsSchema,
		ExampleParameters: exampleParams,
	}
}

// GetChartLines returns chart lines for DSP strategy (predicted values, min, max).
func (s *DSPStrategy) GetChartLines(
	ctx context.Context,
	metrics []prometheus.ContainerMetrics,
	currentResources map[string]autoscalingv1alpha1.ResourceRequirements,
	recommendations []Recommendation,
	config StrategyConfig,
) (map[string]map[int64]float64, error) {
	lines := make(map[string]map[int64]float64)

	for _, rec := range recommendations {
		containerName := rec.ContainerName

		// Find metrics for this container
		containerMetrics := findContainerMetrics(metrics, containerName)
		if containerMetrics == nil {
			continue
		}

		// Convert metrics to time series
		cpuSeries, memSeries, err := s.extractTimeSeries(*containerMetrics)
		if err != nil {
			continue
		}

		// Get DSP parameters
		params := s.getDSPParameters(config)

		// Process CPU chart lines
		s.addResourceChartLines(lines, cpuSeries, params, resourceTypeCPU, config)

		// Process Memory chart lines
		s.addResourceChartLines(lines, memSeries, params, resourceTypeMemory, config)
	}

	return lines, nil
}

// findContainerMetrics finds metrics for a specific container.
func findContainerMetrics(metrics []prometheus.ContainerMetrics, containerName string) *prometheus.ContainerMetrics {
	for i := range metrics {
		if metrics[i].ContainerName == containerName {
			return &metrics[i]
		}
	}
	return nil
}

// addResourceChartLines adds chart lines for a specific resource type (cpu or memory).
func (s *DSPStrategy) addResourceChartLines(
	lines map[string]map[int64]float64,
	series []preprocessing.TimeSeries,
	params dsp.FFTConfig,
	resourceType string,
	config StrategyConfig,
) {
	if len(series) == 0 {
		return
	}

	pred, _, err := s.predictResource(series, params, resourceType, config)
	if err != nil || pred == nil {
		return
	}

	latestTs := series[len(series)-1].Timestamp

	// Add predicted requests and limits
	s.addPredictionLines(lines, pred, resourceType, latestTs)

	// Add min/max from time series
	s.addMinMaxLines(lines, series, resourceType, latestTs)
}

// addPredictionLines adds predicted request/limit lines for a resource type.
func (s *DSPStrategy) addPredictionLines(
	lines map[string]map[int64]float64,
	pred *ResourcePrediction,
	resourceType string,
	timestamp int64,
) {
	reqKey := resourceType + "_predicted_requests"
	limKey := resourceType + "_predicted_limits"

	if lines[reqKey] == nil {
		lines[reqKey] = make(map[int64]float64)
	}
	if lines[limKey] == nil {
		lines[limKey] = make(map[int64]float64)
	}

	if pred.Requests != nil {
		if resourceType == resourceTypeCPU {
			lines[reqKey][timestamp] = float64(pred.Requests.MilliValue()) / 1000.0
		} else {
			lines[reqKey][timestamp] = float64(pred.Requests.Value())
		}
	}
	if pred.Limits != nil {
		if resourceType == resourceTypeCPU {
			lines[limKey][timestamp] = float64(pred.Limits.MilliValue()) / 1000.0
		} else {
			lines[limKey][timestamp] = float64(pred.Limits.Value())
		}
	}
}

// addMinMaxLines adds min/max lines for a resource type.
func (s *DSPStrategy) addMinMaxLines(
	lines map[string]map[int64]float64,
	series []preprocessing.TimeSeries,
	resourceType string,
	timestamp int64,
) {
	minKey := resourceType + "_min"
	maxKey := resourceType + "_max"

	if lines[minKey] == nil {
		lines[minKey] = make(map[int64]float64)
	}
	if lines[maxKey] == nil {
		lines[maxKey] = make(map[int64]float64)
	}

	minVal, maxVal := getMinMax(series)
	lines[minKey][timestamp] = minVal
	lines[maxKey][timestamp] = maxVal
}

// getMinMax returns the min and max values from a time series.
func getMinMax(series []preprocessing.TimeSeries) (min, max float64) {
	if len(series) == 0 {
		return 0, 0
	}
	min = series[0].Value
	max = series[0].Value
	for _, point := range series {
		if point.Value < min {
			min = point.Value
		}
		if point.Value > max {
			max = point.Value
		}
	}
	return min, max
}
