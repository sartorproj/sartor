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
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/metrics/prometheus"
)

// Strategy is the interface that all recommendation strategies must implement.
type Strategy interface {
	// Name returns the unique name of the strategy.
	Name() string

	// Calculate computes resource recommendations based on metrics and configuration.
	Calculate(
		ctx context.Context,
		metrics []prometheus.ContainerMetrics,
		currentResources map[string]autoscalingv1alpha1.ResourceRequirements,
		config StrategyConfig,
	) ([]Recommendation, error)

	// ValidateConfig validates the strategy-specific configuration.
	ValidateConfig(config StrategyConfig) error

	// ValidateFitProfileParameters validates FitProfile parameters for this strategy.
	// Returns nil if valid, or an error describing validation failures.
	// Parameters are provided as a map parsed from the FitProfile's RawExtension.
	ValidateFitProfileParameters(parameters map[string]interface{}) error

	// GetDefaultFitProfileSpec returns the default FitProfile spec for this strategy.
	// This helps users understand what parameters are expected.
	GetDefaultFitProfileSpec() FitProfileSpec

	// GetChartLines returns additional lines to display on resource charts.
	// This allows strategies to show min, max, predictions, suggestions, etc.
	// Returns a map of line name to values (timestamp -> value).
	GetChartLines(
		ctx context.Context,
		metrics []prometheus.ContainerMetrics,
		currentResources map[string]autoscalingv1alpha1.ResourceRequirements,
		recommendations []Recommendation,
		config StrategyConfig,
	) (map[string]map[int64]float64, error)
}

// FitProfileSpec defines the structure for a FitProfile configuration.
type FitProfileSpec struct {
	// Strategy is the strategy name.
	Strategy string

	// DisplayName is a human-readable name.
	DisplayName string

	// Description provides a description.
	Description string

	// ParametersSchema describes the expected parameters structure.
	// This is a JSON schema or documentation of expected fields.
	ParametersSchema map[string]interface{}

	// ExampleParameters provides example parameter values.
	ExampleParameters map[string]interface{}
}

// StrategyConfig contains configuration for a recommendation strategy.
type StrategyConfig struct {
	// Intent defines the optimization profile (Eco, Balanced, Critical).
	Intent autoscalingv1alpha1.Intent

	// SafetyRails defines minimum and maximum resource thresholds.
	SafetyRails SafetyRailsConfig

	// ResourceQuota defines namespace resource quota limits.
	ResourceQuota *ResourceQuotaConfig

	// AnalysisWindow is the time window for metrics analysis.
	AnalysisWindow time.Duration

	// Strategy-specific parameters (e.g., DSP parameters, percentile values).
	Parameters map[string]interface{}

	// EnablePeakDetection enables advanced peak detection.
	EnablePeakDetection bool

	// EnableNormalization enables time series normalization.
	EnableNormalization bool

	// EnableOutlierRemoval enables outlier detection and removal.
	EnableOutlierRemoval bool
}

// SafetyRailsConfig defines resource safety limits.
type SafetyRailsConfig struct {
	MinCPU    *resource.Quantity
	MinMemory *resource.Quantity
	MaxCPU    *resource.Quantity
	MaxMemory *resource.Quantity
}

// ResourceQuotaConfig defines namespace quota limits.
type ResourceQuotaConfig struct {
	CPULimit    *resource.Quantity
	MemoryLimit *resource.Quantity
}

// Recommendation holds the calculated recommendation for a container.
type Recommendation struct {
	ContainerName string
	Requests      ResourceValues
	Limits        ResourceValues
	// Confidence indicates the confidence level of the recommendation (0-1).
	Confidence float64
	// Method indicates the method used for this recommendation.
	Method string
	// Capped indicates if the recommendation was capped by safety rails or quotas.
	Capped bool
	// CappedReason provides the reason for capping.
	CappedReason string
	// Metadata contains strategy-specific metadata.
	Metadata map[string]interface{}
}

// ResourceValues holds CPU and Memory values.
type ResourceValues struct {
	CPU    *resource.Quantity
	Memory *resource.Quantity
}

// StrategyRegistry manages available recommendation strategies.
type StrategyRegistry struct {
	strategies map[string]Strategy
}

// NewStrategyRegistry creates a new strategy registry.
func NewStrategyRegistry() *StrategyRegistry {
	return &StrategyRegistry{
		strategies: make(map[string]Strategy),
	}
}

// Register registers a strategy with the registry.
func (r *StrategyRegistry) Register(strategy Strategy) {
	r.strategies[strategy.Name()] = strategy
}

// Get retrieves a strategy by name.
func (r *StrategyRegistry) Get(name string) (Strategy, bool) {
	strategy, exists := r.strategies[name]
	return strategy, exists
}

// List returns all registered strategy names.
func (r *StrategyRegistry) List() []string {
	names := make([]string, 0, len(r.strategies))
	for name := range r.strategies {
		names = append(names, name)
	}
	return names
}

// DefaultRegistry is the global default strategy registry.
var DefaultRegistry = NewStrategyRegistry()
