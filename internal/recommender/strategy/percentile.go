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
)

const (
	cappedAtQuotaCPU    = "capped at ResourceQuota CPU limit"
	cappedAtQuotaMemory = "capped at ResourceQuota memory limit"
)

// PercentileStrategy implements percentile-based recommendation (P95/P99).
// This is the default strategy used by Sartor.
type PercentileStrategy struct {
	intentConfigs map[autoscalingv1alpha1.Intent]IntentConfig
}

// IntentConfig defines multipliers for each intent profile.
type IntentConfig struct {
	RequestsBufferPercent float64
	LimitsBufferPercent   float64
	MaxChangePercent      float64
}

// DefaultIntentConfigs provides default configurations.
var DefaultIntentConfigs = map[autoscalingv1alpha1.Intent]IntentConfig{
	autoscalingv1alpha1.IntentEco: {
		RequestsBufferPercent: 10,
		LimitsBufferPercent:   20,
		MaxChangePercent:      100,
	},
	autoscalingv1alpha1.IntentBalanced: {
		RequestsBufferPercent: 20,
		LimitsBufferPercent:   30,
		MaxChangePercent:      50,
	},
	autoscalingv1alpha1.IntentCritical: {
		RequestsBufferPercent: 40,
		LimitsBufferPercent:   50,
		MaxChangePercent:      30,
	},
}

// NewPercentileStrategy creates a new percentile-based strategy.
func NewPercentileStrategy() *PercentileStrategy {
	return &PercentileStrategy{
		intentConfigs: DefaultIntentConfigs,
	}
}

// Name returns the strategy name.
func (s *PercentileStrategy) Name() string {
	return "percentile"
}

// Calculate computes recommendations using P95 for requests and P99 for limits.
func (s *PercentileStrategy) Calculate(
	ctx context.Context,
	metrics []prometheus.ContainerMetrics,
	currentResources map[string]autoscalingv1alpha1.ResourceRequirements,
	config StrategyConfig,
) ([]Recommendation, error) {
	// Extract intent config from parameters if provided
	intentConfig := s.getIntentConfigFromParameters(config)

	recommendations := make([]Recommendation, 0, len(metrics))

	for _, m := range metrics {
		rec := s.calculateForContainer(m, currentResources[m.ContainerName], intentConfig, config)
		recommendations = append(recommendations, rec)
	}

	return recommendations, nil
}

// getIntentConfigFromParameters extracts IntentConfig from strategy parameters.
func (s *PercentileStrategy) getIntentConfigFromParameters(config StrategyConfig) IntentConfig {
	// Default to Balanced if no parameters
	intentConfig := s.intentConfigs[autoscalingv1alpha1.IntentBalanced]

	if config.Parameters != nil {
		if requestsBuffer, ok := config.Parameters["requestsBufferPercent"].(float64); ok {
			intentConfig.RequestsBufferPercent = requestsBuffer
		}
		if limitsBuffer, ok := config.Parameters["limitsBufferPercent"].(float64); ok {
			intentConfig.LimitsBufferPercent = limitsBuffer
		}
		if maxChange, ok := config.Parameters["maxChangePercent"].(float64); ok {
			intentConfig.MaxChangePercent = maxChange
		}
	}

	// If Intent is specified, use it as base (for backward compatibility)
	if config.Intent != "" {
		if baseConfig, exists := s.intentConfigs[config.Intent]; exists {
			intentConfig = baseConfig
			// Override with parameters if provided
			if config.Parameters != nil {
				if requestsBuffer, ok := config.Parameters["requestsBufferPercent"].(float64); ok {
					intentConfig.RequestsBufferPercent = requestsBuffer
				}
				if limitsBuffer, ok := config.Parameters["limitsBufferPercent"].(float64); ok {
					intentConfig.LimitsBufferPercent = limitsBuffer
				}
				if maxChange, ok := config.Parameters["maxChangePercent"].(float64); ok {
					intentConfig.MaxChangePercent = maxChange
				}
			}
		}
	}

	return intentConfig
}

// calculateForContainer computes recommendation for a single container.
func (s *PercentileStrategy) calculateForContainer(
	metrics prometheus.ContainerMetrics,
	current autoscalingv1alpha1.ResourceRequirements,
	intentConfig IntentConfig,
	config StrategyConfig,
) Recommendation {
	rec := Recommendation{
		ContainerName: metrics.ContainerName,
		Method:        "percentile",
		Confidence:    0.8, // Default confidence for percentile method
		Metadata:      make(map[string]interface{}),
	}

	// Calculate CPU requests (P95 + buffer)
	if metrics.P95CPU != nil {
		cpuRequests := applyBuffer(metrics.P95CPU, intentConfig.RequestsBufferPercent)
		cpuRequests = s.applyGradualChange(cpuRequests, getCPU(current.Requests), intentConfig.MaxChangePercent)
		cpuRequests = s.applySafetyRails(cpuRequests, config.SafetyRails.MinCPU, config.SafetyRails.MaxCPU, &rec)
		rec.Requests.CPU = cpuRequests
		rec.Metadata["cpu_p95"] = metrics.P95CPU.String()
		rec.Metadata["requests_buffer_percent"] = intentConfig.RequestsBufferPercent
	}

	// Calculate CPU limits (P99 + buffer)
	if metrics.P99CPU != nil {
		cpuLimits := applyBuffer(metrics.P99CPU, intentConfig.LimitsBufferPercent)
		cpuLimits = s.applyGradualChange(cpuLimits, getCPU(current.Limits), intentConfig.MaxChangePercent)
		cpuLimits = s.applySafetyRails(cpuLimits, config.SafetyRails.MinCPU, config.SafetyRails.MaxCPU, &rec)
		rec.Limits.CPU = cpuLimits
		rec.Metadata["cpu_p99"] = metrics.P99CPU.String()
		rec.Metadata["limits_buffer_percent"] = intentConfig.LimitsBufferPercent
	}

	// Calculate Memory requests (P95 + buffer)
	if metrics.P95Memory != nil {
		memRequests := applyBuffer(metrics.P95Memory, intentConfig.RequestsBufferPercent)
		memRequests = s.applyGradualChange(memRequests, getMemory(current.Requests), intentConfig.MaxChangePercent)
		memRequests = s.applySafetyRails(memRequests, config.SafetyRails.MinMemory, config.SafetyRails.MaxMemory, &rec)
		rec.Requests.Memory = memRequests
		rec.Metadata["memory_p95"] = metrics.P95Memory.String()
	}

	// Calculate Memory limits (P99 + buffer)
	if metrics.P99Memory != nil {
		memLimits := applyBuffer(metrics.P99Memory, intentConfig.LimitsBufferPercent)
		memLimits = s.applyGradualChange(memLimits, getMemory(current.Limits), intentConfig.MaxChangePercent)
		memLimits = s.applySafetyRails(memLimits, config.SafetyRails.MinMemory, config.SafetyRails.MaxMemory, &rec)
		rec.Limits.Memory = memLimits
		rec.Metadata["memory_p99"] = metrics.P99Memory.String()
	}

	// Apply quota limits
	if config.ResourceQuota != nil {
		s.applyQuotaLimits(&rec, config.ResourceQuota)
	}

	return rec
}

// applyBuffer adds a percentage buffer to a quantity.
func applyBuffer(q *resource.Quantity, bufferPercent float64) *resource.Quantity {
	if q == nil {
		return nil
	}
	value := q.AsApproximateFloat64()
	buffered := value * (1 + bufferPercent/100)

	if q.Format == resource.DecimalSI {
		return resource.NewMilliQuantity(int64(buffered*1000), resource.DecimalSI)
	}
	return resource.NewQuantity(int64(buffered), resource.BinarySI)
}

// applyGradualChange limits the change to maxChangePercent from current.
func (s *PercentileStrategy) applyGradualChange(recommended, current *resource.Quantity, maxChangePercent float64) *resource.Quantity {
	if current == nil || recommended == nil || maxChangePercent >= 100 {
		return recommended
	}

	currentValue := current.AsApproximateFloat64()
	recommendedValue := recommended.AsApproximateFloat64()

	if currentValue == 0 {
		return recommended
	}

	changePercent := ((recommendedValue - currentValue) / currentValue) * 100

	if changePercent > maxChangePercent {
		cappedValue := currentValue * (1 + maxChangePercent/100)
		if recommended.Format == resource.DecimalSI {
			return resource.NewMilliQuantity(int64(cappedValue*1000), resource.DecimalSI)
		}
		return resource.NewQuantity(int64(cappedValue), resource.BinarySI)
	} else if changePercent < -maxChangePercent {
		cappedValue := currentValue * (1 - maxChangePercent/100)
		if recommended.Format == resource.DecimalSI {
			return resource.NewMilliQuantity(int64(cappedValue*1000), resource.DecimalSI)
		}
		return resource.NewQuantity(int64(cappedValue), resource.BinarySI)
	}

	return recommended
}

// applySafetyRails ensures the value is within min/max bounds.
func (s *PercentileStrategy) applySafetyRails(q, min, max *resource.Quantity, rec *Recommendation) *resource.Quantity {
	if q == nil {
		return nil
	}

	result := q.DeepCopy()

	if min != nil && result.Cmp(*min) < 0 {
		result = min.DeepCopy()
		rec.Capped = true
		rec.CappedReason = "capped at minimum safety rail"
	}

	if max != nil && result.Cmp(*max) > 0 {
		result = max.DeepCopy()
		rec.Capped = true
		rec.CappedReason = "capped at maximum safety rail"
	}

	return &result
}

// applyQuotaLimits caps recommendations at quota limits.
func (s *PercentileStrategy) applyQuotaLimits(rec *Recommendation, quota *ResourceQuotaConfig) {
	if quota == nil {
		return
	}

	if quota.CPULimit != nil {
		if rec.Requests.CPU != nil && rec.Requests.CPU.Cmp(*quota.CPULimit) > 0 {
			cpuCopy := quota.CPULimit.DeepCopy()
			rec.Requests.CPU = &cpuCopy
			rec.Capped = true
			rec.CappedReason = cappedAtQuotaCPU
		}
		if rec.Limits.CPU != nil && rec.Limits.CPU.Cmp(*quota.CPULimit) > 0 {
			cpuCopy := quota.CPULimit.DeepCopy()
			rec.Limits.CPU = &cpuCopy
			rec.Capped = true
			rec.CappedReason = cappedAtQuotaCPU
		}
	}

	if quota.MemoryLimit != nil {
		if rec.Requests.Memory != nil && rec.Requests.Memory.Cmp(*quota.MemoryLimit) > 0 {
			memCopy := quota.MemoryLimit.DeepCopy()
			rec.Requests.Memory = &memCopy
			rec.Capped = true
			rec.CappedReason = cappedAtQuotaMemory
		}
		if rec.Limits.Memory != nil && rec.Limits.Memory.Cmp(*quota.MemoryLimit) > 0 {
			memCopy := quota.MemoryLimit.DeepCopy()
			rec.Limits.Memory = &memCopy
			rec.Capped = true
			rec.CappedReason = cappedAtQuotaMemory
		}
	}
}

// ValidateConfig validates the strategy configuration.
func (s *PercentileStrategy) ValidateConfig(config StrategyConfig) error {
	// Intent is optional now (can use FitProfile instead)
	return nil
}

// ValidateFitProfileParameters validates FitProfile parameters for percentile strategy.
func (s *PercentileStrategy) ValidateFitProfileParameters(parameters map[string]interface{}) error {
	if parameters == nil {
		return nil // Parameters are optional
	}

	// Validate requestsBufferPercent
	if requestsBuffer, ok := parameters["requestsBufferPercent"].(float64); ok {
		if requestsBuffer < 0 || requestsBuffer > 1000 {
			return fmt.Errorf("requestsBufferPercent must be between 0 and 1000, got %f", requestsBuffer)
		}
	}

	// Validate limitsBufferPercent
	if limitsBuffer, ok := parameters["limitsBufferPercent"].(float64); ok {
		if limitsBuffer < 0 || limitsBuffer > 1000 {
			return fmt.Errorf("limitsBufferPercent must be between 0 and 1000, got %f", limitsBuffer)
		}
	}

	// Validate maxChangePercent
	if maxChange, ok := parameters["maxChangePercent"].(float64); ok {
		if maxChange < 0 || maxChange > 1000 {
			return fmt.Errorf("maxChangePercent must be between 0 and 1000, got %f", maxChange)
		}
	}

	return nil
}

// GetDefaultFitProfileSpec returns the default FitProfile spec for percentile strategy.
func (s *PercentileStrategy) GetDefaultFitProfileSpec() FitProfileSpec {
	exampleParams := map[string]interface{}{
		"requestsBufferPercent": 20.0,
		"limitsBufferPercent":   30.0,
		"maxChangePercent":      50.0,
	}

	var paramsSchema map[string]interface{}
	_ = json.Unmarshal([]byte(`{
		"type": "object",
		"properties": {
			"requestsBufferPercent": {
				"type": "number",
				"description": "Buffer percentage for requests (P95)",
				"minimum": 0,
				"maximum": 1000,
				"default": 20
			},
			"limitsBufferPercent": {
				"type": "number",
				"description": "Buffer percentage for limits (P99)",
				"minimum": 0,
				"maximum": 1000,
				"default": 30
			},
			"maxChangePercent": {
				"type": "number",
				"description": "Maximum change percentage per recommendation",
				"minimum": 0,
				"maximum": 1000,
				"default": 50
			}
		}
	}`), &paramsSchema)

	return FitProfileSpec{
		Strategy:          "percentile",
		DisplayName:       "Percentile-based Optimization",
		Description:       "Uses P95 for requests and P99 for limits with configurable buffers",
		ParametersSchema:  paramsSchema,
		ExampleParameters: exampleParams,
	}
}

// GetChartLines returns chart lines for percentile strategy (P95 requests, P99 limits).
func (s *PercentileStrategy) GetChartLines(
	ctx context.Context,
	metrics []prometheus.ContainerMetrics,
	currentResources map[string]autoscalingv1alpha1.ResourceRequirements,
	recommendations []Recommendation,
	config StrategyConfig,
) (map[string]map[int64]float64, error) {
	lines := make(map[string]map[int64]float64)
	now := time.Now().Unix()

	for _, rec := range recommendations {
		containerName := rec.ContainerName

		// Get P95 and P99 values from metrics
		for _, m := range metrics {
			if m.ContainerName != containerName {
				continue
			}

			// CPU P95 requests line
			if m.P95CPU != nil {
				if lines["cpu_p95_requests"] == nil {
					lines["cpu_p95_requests"] = make(map[int64]float64)
				}
				lines["cpu_p95_requests"][now] = float64(m.P95CPU.MilliValue()) / 1000.0
			}

			// CPU P99 limits line
			if m.P99CPU != nil {
				if lines["cpu_p99_limits"] == nil {
					lines["cpu_p99_limits"] = make(map[int64]float64)
				}
				lines["cpu_p99_limits"][now] = float64(m.P99CPU.MilliValue()) / 1000.0
			}

			// Memory P95 requests line
			if m.P95Memory != nil {
				if lines["memory_p95_requests"] == nil {
					lines["memory_p95_requests"] = make(map[int64]float64)
				}
				lines["memory_p95_requests"][now] = float64(m.P95Memory.Value())
			}

			// Memory P99 limits line
			if m.P99Memory != nil {
				if lines["memory_p99_limits"] == nil {
					lines["memory_p99_limits"] = make(map[int64]float64)
				}
				lines["memory_p99_limits"][now] = float64(m.P99Memory.Value())
			}
		}
	}

	return lines, nil
}

// Helper functions
func getCPU(rv *autoscalingv1alpha1.ResourceValues) *resource.Quantity {
	if rv == nil {
		return nil
	}
	return rv.CPU
}

func getMemory(rv *autoscalingv1alpha1.ResourceValues) *resource.Quantity {
	if rv == nil {
		return nil
	}
	return rv.Memory
}
