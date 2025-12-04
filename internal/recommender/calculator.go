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

package recommender

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/metrics/prometheus"
	"github.com/sartorproj/sartor/internal/recommender/strategy"
)

// Calculator computes resource recommendations using pluggable strategies.
type Calculator struct {
	registry *strategy.StrategyRegistry
}

// NewCalculator creates a new recommendation calculator with default strategies.
func NewCalculator() *Calculator {
	calc := &Calculator{
		registry: strategy.NewStrategyRegistry(),
	}

	// Register default strategies
	calc.registry.Register(strategy.NewPercentileStrategy())
	calc.registry.Register(strategy.NewDSPStrategy(60 * time.Second))

	return calc
}

// RegisterStrategy registers a custom recommendation strategy.
func (c *Calculator) RegisterStrategy(s strategy.Strategy) {
	c.registry.Register(s)
}

// GetRegistry returns the strategy registry.
func (c *Calculator) GetRegistry() *strategy.StrategyRegistry {
	return c.registry
}

// Calculate computes recommendations using the specified strategy.
func (c *Calculator) Calculate(
	ctx context.Context,
	metrics []prometheus.ContainerMetrics,
	currentResources map[string]autoscalingv1alpha1.ResourceRequirements,
	intent autoscalingv1alpha1.Intent,
	safetyRails SafetyRails,
	quota *ResourceQuota,
	analysisWindow time.Duration,
	strategyName string,
	strategyParams map[string]interface{},
	enablePeakDetection bool,
	enableNormalization bool,
) ([]strategy.Recommendation, error) {
	// Get strategy
	s, exists := c.registry.Get(strategyName)
	if !exists {
		// Fallback to percentile if strategy not found
		s, _ = c.registry.Get("percentile")
		if s == nil {
			return nil, fmt.Errorf("no strategy available")
		}
	}

	// Build strategy config
	config := strategy.StrategyConfig{
		Intent:              intent,
		AnalysisWindow:      analysisWindow,
		Parameters:          strategyParams,
		EnablePeakDetection: enablePeakDetection,
		EnableNormalization: enableNormalization,
		SafetyRails: strategy.SafetyRailsConfig{
			MinCPU:    safetyRails.MinCPU,
			MinMemory: safetyRails.MinMemory,
			MaxCPU:    safetyRails.MaxCPU,
			MaxMemory: safetyRails.MaxMemory,
		},
	}

	if quota != nil {
		config.ResourceQuota = &strategy.ResourceQuotaConfig{
			CPULimit:    quota.CPULimit,
			MemoryLimit: quota.MemoryLimit,
		}
	}

	// Validate config
	if err := s.ValidateConfig(config); err != nil {
		return nil, fmt.Errorf("invalid strategy config: %w", err)
	}

	// Calculate recommendations
	return s.Calculate(ctx, metrics, currentResources, config)
}

// SafetyRails defines the minimum and maximum resource thresholds.
type SafetyRails struct {
	MinCPU    *resource.Quantity
	MinMemory *resource.Quantity
	MaxCPU    *resource.Quantity
	MaxMemory *resource.Quantity
}

// ResourceQuota defines namespace resource quota limits.
type ResourceQuota struct {
	CPULimit    *resource.Quantity
	MemoryLimit *resource.Quantity
}

// Recommendation holds the calculated recommendation for a container.
// Deprecated: Use strategy.Recommendation instead.
type Recommendation struct {
	ContainerName string
	Requests      ResourceValues
	Limits        ResourceValues
	Capped        bool
	CappedReason  string
}

// ResourceValues holds CPU and Memory values.
// Deprecated: Use strategy.ResourceValues instead.
type ResourceValues struct {
	CPU    *resource.Quantity
	Memory *resource.Quantity
}

// ShouldCreateCut determines if a Cut should be created based on recommendations.
func ShouldCreateCut(
	recommendations []strategy.Recommendation,
	currentResources map[string]autoscalingv1alpha1.ResourceRequirements,
	minChangePercent int32,
) bool {
	for _, rec := range recommendations {
		current, exists := currentResources[rec.ContainerName]
		if !exists {
			// No current resources - always create cut
			return true
		}

		// Check CPU change
		if rec.Requests.CPU != nil && current.Requests != nil && current.Requests.CPU != nil {
			if hasSignificantChange(current.Requests.CPU, rec.Requests.CPU, minChangePercent) {
				return true
			}
		}

		// Check Memory change
		if rec.Requests.Memory != nil && current.Requests != nil && current.Requests.Memory != nil {
			if hasSignificantChange(current.Requests.Memory, rec.Requests.Memory, minChangePercent) {
				return true
			}
		}
	}

	return false
}

// hasSignificantChange checks if the change exceeds the minimum threshold.
func hasSignificantChange(current, recommended *resource.Quantity, minChangePercent int32) bool {
	if current == nil || recommended == nil {
		return true
	}

	currentValue := current.AsApproximateFloat64()
	recommendedValue := recommended.AsApproximateFloat64()

	if currentValue == 0 {
		return recommendedValue != 0
	}

	changePercent := ((recommendedValue - currentValue) / currentValue) * 100
	return abs(changePercent) >= float64(minChangePercent)
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
