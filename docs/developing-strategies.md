# Developing Custom Recommendation Strategies

Sartor's recommendation engine uses a plugin-based architecture that allows you to develop and register custom strategies for resource optimization. This guide explains how to create your own strategy.

## Strategy Interface

All strategies must implement the `strategy.Strategy` interface:

```go
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
    ValidateFitProfileParameters(parameters map[string]interface{}) error

    // GetDefaultFitProfileSpec returns the default FitProfile spec for this strategy.
    GetDefaultFitProfileSpec() FitProfileSpec
}
```

## Step-by-Step Guide

### 1. Create Your Strategy Struct

Create a new file for your strategy, e.g., `internal/recommender/strategy/mystrategy.go`:

```go
package strategy

import (
    "context"
    "encoding/json"
    "fmt"
    
    "k8s.io/apimachinery/pkg/api/resource"
    autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
    "github.com/sartorproj/sartor/internal/metrics/prometheus"
)

// MyStrategy implements a custom recommendation strategy.
type MyStrategy struct {
    // Add any internal state here
}

// NewMyStrategy creates a new instance of MyStrategy.
func NewMyStrategy() *MyStrategy {
    return &MyStrategy{}
}
```

### 2. Implement the Name() Method

```go
func (s *MyStrategy) Name() string {
    return "mystrategy"
}
```

### 3. Implement the Calculate() Method

This is where your core recommendation logic lives:

```go
func (s *MyStrategy) Calculate(
    ctx context.Context,
    metrics []prometheus.ContainerMetrics,
    currentResources map[string]autoscalingv1alpha1.ResourceRequirements,
    config StrategyConfig,
) ([]Recommendation, error) {
    recommendations := make([]Recommendation, 0, len(metrics))

    for _, m := range metrics {
        rec := Recommendation{
            ContainerName: m.ContainerName,
            Method:        "mystrategy",
            Confidence:    0.9, // Your confidence level
            Metadata:      make(map[string]interface{}),
        }

        // Extract parameters from config
        multiplier := 1.2 // default
        if config.Parameters != nil {
            if m, ok := config.Parameters["multiplier"].(float64); ok {
                multiplier = m
            }
        }

        // Your custom calculation logic here
        // Example: Use P95 with a multiplier
        if m.P95CPU != nil {
            cpuValue := m.P95CPU.AsApproximateFloat64() * multiplier
            rec.Requests.CPU = resource.NewMilliQuantity(
                int64(cpuValue*1000),
                resource.DecimalSI,
            )
        }

        if m.P95Memory != nil {
            memValue := m.P95Memory.AsApproximateFloat64() * multiplier
            rec.Requests.Memory = resource.NewQuantity(
                int64(memValue),
                resource.BinarySI,
            )
        }

        // Apply safety rails
        rec.Requests.CPU = s.applySafetyRails(
            rec.Requests.CPU,
            config.SafetyRails.MinCPU,
            config.SafetyRails.MaxCPU,
        )
        rec.Requests.Memory = s.applySafetyRails(
            rec.Requests.Memory,
            config.SafetyRails.MinMemory,
            config.SafetyRails.MaxMemory,
        )

        // Apply resource quota limits
        if config.ResourceQuota != nil {
            s.applyQuotaLimits(&rec, config.ResourceQuota)
        }

        recommendations = append(recommendations, rec)
    }

    return recommendations, nil
}

func (s *MyStrategy) applySafetyRails(
    q *resource.Quantity,
    min, max *resource.Quantity,
) *resource.Quantity {
    if q == nil {
        return nil
    }
    result := q.DeepCopy()
    if min != nil && result.Cmp(*min) < 0 {
        result = min.DeepCopy()
    }
    if max != nil && result.Cmp(*max) > 0 {
        result = max.DeepCopy()
    }
    return &result
}

func (s *MyStrategy) applyQuotaLimits(
    rec *Recommendation,
    quota *ResourceQuotaConfig,
) {
    if quota.CPULimit != nil && rec.Requests.CPU != nil {
        if rec.Requests.CPU.Cmp(*quota.CPULimit) > 0 {
            cpuCopy := quota.CPULimit.DeepCopy()
            rec.Requests.CPU = &cpuCopy
            rec.Capped = true
            rec.CappedReason = "capped at ResourceQuota CPU limit"
        }
    }
    // Similar for memory...
}
```

### 4. Implement ValidateConfig()

```go
func (s *MyStrategy) ValidateConfig(config StrategyConfig) error {
    // Validate any required config fields
    // Intent is optional now (can use FitProfile instead)
    return nil
}
```

### 5. Implement ValidateFitProfileParameters()

This validates the parameters that users can set in a FitProfile:

```go
func (s *MyStrategy) ValidateFitProfileParameters(parameters map[string]interface{}) error {
    if parameters == nil {
        return nil // Parameters are optional
    }

    // Validate multiplier
    if multiplier, ok := parameters["multiplier"].(float64); ok {
        if multiplier < 0.1 || multiplier > 10.0 {
            return fmt.Errorf("multiplier must be between 0.1 and 10.0, got %f", multiplier)
        }
    }

    // Validate other parameters...
    return nil
}
```

### 6. Implement GetDefaultFitProfileSpec()

This documents what parameters your strategy expects:

```go
func (s *MyStrategy) GetDefaultFitProfileSpec() FitProfileSpec {
    exampleParams := map[string]interface{}{
        "multiplier": 1.2,
        "useP99":     false,
    }

    var paramsSchema map[string]interface{}
    json.Unmarshal([]byte(`{
        "type": "object",
        "properties": {
            "multiplier": {
                "type": "number",
                "description": "Multiplier for resource recommendations",
                "minimum": 0.1,
                "maximum": 10.0,
                "default": 1.2
            },
            "useP99": {
                "type": "boolean",
                "description": "Use P99 instead of P95 for requests",
                "default": false
            }
        }
    }`), &paramsSchema)

    return FitProfileSpec{
        Strategy:         "mystrategy",
        DisplayName:      "My Custom Strategy",
        Description:      "A custom strategy that uses multipliers",
        ParametersSchema: paramsSchema,
        ExampleParameters: exampleParams,
    }
}
```

### 7. Register Your Strategy

In `cmd/controller/main.go`, register your strategy:

```go
// Create strategy registry and register default strategies
strategyRegistry := strategy.NewStrategyRegistry()
strategyRegistry.Register(strategy.NewPercentileStrategy())
strategyRegistry.Register(strategy.NewDSPStrategy(60))
strategyRegistry.Register(strategy.NewMyStrategy()) // Add your strategy

if err := (&controller.FitProfileReconciler{
    Client:          mgr.GetClient(),
    Scheme:          mgr.GetScheme(),
    StrategyRegistry: strategyRegistry,
}).SetupWithManager(mgr); err != nil {
    setupLog.Error(err, "unable to create controller", "controller", "FitProfile")
    os.Exit(1)
}
```

### 8. Create a FitProfile for Your Strategy

Users can now create FitProfiles that use your strategy:

```yaml
apiVersion: autoscaling.sartorproj.io/v1alpha1
kind: FitProfile
metadata:
  name: my-strategy-profile
  namespace: default
spec:
  strategy: mystrategy
  displayName: "My Custom Strategy Profile"
  description: "Uses my custom strategy with specific parameters"
  parameters:
    multiplier: 1.5
    useP99: true
```

## Available Metrics

The `prometheus.ContainerMetrics` struct provides:

- `ContainerName`: Name of the container
- `P95CPU`: 95th percentile CPU usage
- `P99CPU`: 99th percentile CPU usage
- `P95Memory`: 95th percentile memory usage
- `P99Memory`: 99th percentile memory usage
- `TimeSeries`: Full time series data (if available)

## StrategyConfig Fields

The `StrategyConfig` passed to `Calculate()` contains:

- `Intent`: Legacy intent (Eco/Balanced/Critical) - may be empty
- `AnalysisWindow`: Time window for analysis
- `Parameters`: Strategy-specific parameters from FitProfile
- `EnablePeakDetection`: Whether peak detection is enabled
- `EnableNormalization`: Whether normalization is enabled
- `SafetyRails`: Min/Max CPU and Memory limits
- `ResourceQuota`: Namespace quota limits

## Best Practices

1. **Always apply safety rails**: Use the provided safety rails to prevent dangerous recommendations
2. **Respect quotas**: Check and apply ResourceQuota limits
3. **Provide metadata**: Add useful information to `Recommendation.Metadata` for debugging
4. **Set confidence**: Provide a realistic confidence score (0-1)
5. **Handle missing data**: Gracefully handle cases where metrics might be missing
6. **Validate parameters**: Thoroughly validate FitProfile parameters
7. **Document parameters**: Provide clear schema and examples in `GetDefaultFitProfileSpec()`

## Example: Complete Strategy

See `internal/recommender/strategy/percentile.go` and `internal/recommender/strategy/dsp.go` for complete, production-ready examples.

## Testing Your Strategy

1. **Unit tests**: Create tests for your calculation logic
2. **Integration tests**: Test with real metrics data
3. **FitProfile validation**: Test that your parameter validation works correctly

Example test:

```go
func TestMyStrategy_Calculate(t *testing.T) {
    strategy := NewMyStrategy()
    
    metrics := []prometheus.ContainerMetrics{
        {
            ContainerName: "app",
            P95CPU:        resource.MustParse("100m"),
            P95Memory:     resource.MustParse("128Mi"),
        },
    }
    
    config := StrategyConfig{
        Parameters: map[string]interface{}{
            "multiplier": 1.2,
        },
        SafetyRails: SafetyRailsConfig{
            MinCPU: resource.MustParse("50m"),
        },
    }
    
    recs, err := strategy.Calculate(context.Background(), metrics, nil, config)
    assert.NoError(t, err)
    assert.Len(t, recs, 1)
    // Add more assertions...
}
```

## External Strategy Plugins

For advanced use cases, you can develop strategies as separate Go packages and import them:

1. Create a separate Go module for your strategy
2. Implement the `strategy.Strategy` interface
3. Export an initialization function
4. Import and register in the controller

This allows strategies to be developed independently and even distributed separately.
