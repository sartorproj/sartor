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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/metrics/prometheus"
	"github.com/sartorproj/sartor/internal/recommender/strategy"
)

func TestNewCalculator(t *testing.T) {
	calc := NewCalculator()
	require.NotNil(t, calc)
	assert.NotNil(t, calc.GetRegistry())
}

func TestCalculator_Calculate(t *testing.T) {
	calc := NewCalculator()
	ctx := context.Background()

	metrics := []prometheus.ContainerMetrics{
		{
			ContainerName: "app",
			P95CPU:        resourcePtr("100m"),
			P99CPU:        resourcePtr("150m"),
			P95Memory:     resourcePtr("128Mi"),
			P99Memory:     resourcePtr("192Mi"),
		},
	}

	currentResources := map[string]autoscalingv1alpha1.ResourceRequirements{
		"app": {
			Requests: &autoscalingv1alpha1.ResourceValues{
				CPU:    resourcePtr("200m"),
				Memory: resourcePtr("256Mi"),
			},
			Limits: &autoscalingv1alpha1.ResourceValues{
				CPU:    resourcePtr("500m"),
				Memory: resourcePtr("512Mi"),
			},
		},
	}

	recommendations, err := calc.Calculate(
		ctx,
		metrics,
		currentResources,
		autoscalingv1alpha1.IntentBalanced,
		SafetyRails{},
		nil,
		time.Hour,
		"percentile",
		make(map[string]interface{}),
		false,
		false,
	)

	require.NoError(t, err)
	require.Len(t, recommendations, 1)
	rec := recommendations[0]
	assert.Equal(t, "app", rec.ContainerName)
	assert.NotNil(t, rec.Requests.CPU)
	assert.NotNil(t, rec.Requests.Memory)
	assert.NotNil(t, rec.Limits.CPU)
	assert.NotNil(t, rec.Limits.Memory)
}

func TestCalculator_CalculateWithSafetyRails(t *testing.T) {
	safetyRails := SafetyRails{
		MinCPU:    resourcePtr("50m"),
		MaxCPU:    resourcePtr("2"),
		MinMemory: resourcePtr("64Mi"),
		MaxMemory: resourcePtr("4Gi"),
	}
	calc := NewCalculator()
	ctx := context.Background()

	metrics := []prometheus.ContainerMetrics{
		{
			ContainerName: "app",
			P95CPU:        resourcePtr("10m"),  // Below minimum
			P99CPU:        resourcePtr("5"),    // Above maximum
			P95Memory:     resourcePtr("32Mi"), // Below minimum
			P99Memory:     resourcePtr("8Gi"),  // Above maximum
		},
	}

	currentResources := map[string]autoscalingv1alpha1.ResourceRequirements{
		"app": {},
	}

	recommendations, err := calc.Calculate(
		ctx,
		metrics,
		currentResources,
		autoscalingv1alpha1.IntentBalanced,
		safetyRails,
		nil,
		time.Hour,
		"percentile",
		make(map[string]interface{}),
		false,
		false,
	)

	require.NoError(t, err)
	require.Len(t, recommendations, 1)
	rec := recommendations[0]

	// Should be capped at minimum
	assert.True(t, rec.Requests.CPU.Cmp(*safetyRails.MinCPU) >= 0)
	assert.True(t, rec.Requests.Memory.Cmp(*safetyRails.MinMemory) >= 0)

	// Should be capped at maximum
	assert.True(t, rec.Limits.CPU.Cmp(*safetyRails.MaxCPU) <= 0)
	assert.True(t, rec.Limits.Memory.Cmp(*safetyRails.MaxMemory) <= 0)

	assert.True(t, rec.Capped)
	assert.NotEmpty(t, rec.CappedReason)
}

func TestCalculator_CalculateWithQuota(t *testing.T) {
	quota := &ResourceQuota{
		CPULimit:    resourcePtr("1"),
		MemoryLimit: resourcePtr("2Gi"),
	}
	calc := NewCalculator()
	ctx := context.Background()

	metrics := []prometheus.ContainerMetrics{
		{
			ContainerName: "app",
			P95CPU:        resourcePtr("2"), // Above quota
			P99CPU:        resourcePtr("2"),
			P95Memory:     resourcePtr("4Gi"), // Above quota
			P99Memory:     resourcePtr("4Gi"),
		},
	}

	currentResources := map[string]autoscalingv1alpha1.ResourceRequirements{
		"app": {},
	}

	recommendations, err := calc.Calculate(
		ctx,
		metrics,
		currentResources,
		autoscalingv1alpha1.IntentBalanced,
		SafetyRails{},
		quota,
		time.Hour,
		"percentile",
		make(map[string]interface{}),
		false,
		false,
	)

	require.NoError(t, err)
	require.Len(t, recommendations, 1)
	rec := recommendations[0]

	// Should be capped at quota
	assert.True(t, rec.Requests.CPU.Cmp(*quota.CPULimit) <= 0)
	assert.True(t, rec.Limits.CPU.Cmp(*quota.CPULimit) <= 0)
	assert.True(t, rec.Requests.Memory.Cmp(*quota.MemoryLimit) <= 0)
	assert.True(t, rec.Limits.Memory.Cmp(*quota.MemoryLimit) <= 0)
}

func TestShouldCreateCut(t *testing.T) {
	tests := []struct {
		name             string
		recommendations  []strategy.Recommendation
		currentResources map[string]autoscalingv1alpha1.ResourceRequirements
		minChangePercent int32
		expected         bool
	}{
		{
			name: "change above threshold",
			recommendations: []strategy.Recommendation{
				{
					ContainerName: "app",
					Requests: strategy.ResourceValues{
						CPU: resourcePtr("150m"),
					},
				},
			},
			currentResources: map[string]autoscalingv1alpha1.ResourceRequirements{
				"app": {
					Requests: &autoscalingv1alpha1.ResourceValues{
						CPU: resourcePtr("100m"),
					},
				},
			},
			minChangePercent: 20,
			expected:         true,
		},
		{
			name: "change below threshold",
			recommendations: []strategy.Recommendation{
				{
					ContainerName: "app",
					Requests: strategy.ResourceValues{
						CPU: resourcePtr("110m"),
					},
				},
			},
			currentResources: map[string]autoscalingv1alpha1.ResourceRequirements{
				"app": {
					Requests: &autoscalingv1alpha1.ResourceValues{
						CPU: resourcePtr("100m"),
					},
				},
			},
			minChangePercent: 20,
			expected:         false,
		},
		{
			name: "no current resources",
			recommendations: []strategy.Recommendation{
				{
					ContainerName: "app",
					Requests: strategy.ResourceValues{
						CPU: resourcePtr("100m"),
					},
				},
			},
			currentResources: map[string]autoscalingv1alpha1.ResourceRequirements{},
			minChangePercent: 20,
			expected:         true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ShouldCreateCut(tt.recommendations, tt.currentResources, tt.minChangePercent)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func resourcePtr(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}
