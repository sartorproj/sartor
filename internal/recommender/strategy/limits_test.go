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
	"fmt"
	"math/rand"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/metrics/prometheus"
)

// assertLimitsCoverRequests checks that no limit is below its request and
// that the raised flags in the metadata match the expectation.
func assertLimitsCoverRequests(t *testing.T, rec Recommendation, wantRaisedCPU, wantRaisedMemory bool) {
	t.Helper()
	for _, r := range []struct {
		name           string
		request, limit *resource.Quantity
		key            string
		wantRaised     bool
	}{
		{"cpu", rec.Requests.CPU, rec.Limits.CPU, MetadataCPULimitRaisedToRequest, wantRaisedCPU},
		{"memory", rec.Requests.Memory, rec.Limits.Memory, MetadataMemoryLimitRaisedToRequest, wantRaisedMemory},
	} {
		if r.request != nil && r.limit != nil && r.limit.Cmp(*r.request) < 0 {
			t.Errorf("limits.%s %s < requests.%s %s", r.name, r.limit, r.name, r.request)
		}
		raised, ok := rec.Metadata[r.key]
		if ok != r.wantRaised || (ok && raised != true) {
			t.Errorf("metadata %s = %v (set: %v), want set: %v", r.key, raised, ok, r.wantRaised)
		}
	}
}

func TestEnsureLimitsCoverRequests(t *testing.T) {
	tests := []struct {
		name               string
		rec                Recommendation
		wantLimCPU         string
		wantLimMem         string
		wantCPU, wantMem   bool
		wantMetadataNilMap bool
	}{
		{
			name: "cpu limit below request is raised",
			rec: Recommendation{
				Requests: ResourceValues{CPU: qty("500m"), Memory: qty("128Mi")},
				Limits:   ResourceValues{CPU: qty("100m"), Memory: qty("256Mi")},
			},
			wantLimCPU: "500m",
			wantLimMem: "256Mi",
			wantCPU:    true,
		},
		{
			name: "memory limit below request is raised",
			rec: Recommendation{
				Requests: ResourceValues{CPU: qty("100m"), Memory: qty("1Gi")},
				Limits:   ResourceValues{CPU: qty("200m"), Memory: qty("512Mi")},
			},
			wantLimCPU: "200m",
			wantLimMem: "1Gi",
			wantMem:    true,
		},
		{
			name: "equal values are left alone",
			rec: Recommendation{
				Requests: ResourceValues{CPU: qty("250m"), Memory: qty("256Mi")},
				Limits:   ResourceValues{CPU: qty("250m"), Memory: qty("256Mi")},
			},
			wantLimCPU:         "250m",
			wantLimMem:         "256Mi",
			wantMetadataNilMap: true,
		},
		{
			name: "equal values in different formats are left alone",
			rec: Recommendation{
				Requests: ResourceValues{CPU: qty("1"), Memory: qty("1Gi")},
				Limits:   ResourceValues{CPU: qty("1000m"), Memory: qty("1073741824")},
			},
			wantLimCPU:         "1",
			wantLimMem:         "1Gi",
			wantMetadataNilMap: true,
		},
		{
			name: "missing limits are not invented",
			rec: Recommendation{
				Requests: ResourceValues{CPU: qty("500m"), Memory: qty("1Gi")},
			},
			wantMetadataNilMap: true,
		},
		{
			name: "missing requests leave limits unchanged",
			rec: Recommendation{
				Limits: ResourceValues{CPU: qty("100m"), Memory: qty("64Mi")},
			},
			wantLimCPU:         "100m",
			wantLimMem:         "64Mi",
			wantMetadataNilMap: true,
		},
		{
			name: "both resources raised",
			rec: Recommendation{
				Requests: ResourceValues{CPU: qty("2"), Memory: qty("2Gi")},
				Limits:   ResourceValues{CPU: qty("1"), Memory: qty("1Gi")},
				Metadata: map[string]interface{}{"cpu_p95": "1"},
			},
			wantLimCPU: "2",
			wantLimMem: "2Gi",
			wantCPU:    true,
			wantMem:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := tt.rec
			reqCPU, reqMem := rec.Requests.CPU, rec.Requests.Memory
			var reqCPUBefore, reqMemBefore string
			if reqCPU != nil {
				reqCPUBefore = reqCPU.String()
			}
			if reqMem != nil {
				reqMemBefore = reqMem.String()
			}

			EnsureLimitsCoverRequests(&rec)

			assertQty(t, "limits.cpu", rec.Limits.CPU, tt.wantLimCPU)
			assertQty(t, "limits.memory", rec.Limits.Memory, tt.wantLimMem)
			assertQty(t, "requests.cpu", rec.Requests.CPU, reqCPUBefore)
			assertQty(t, "requests.memory", rec.Requests.Memory, reqMemBefore)
			if tt.wantMetadataNilMap && rec.Metadata != nil {
				t.Errorf("metadata = %v, want untouched nil map", rec.Metadata)
			}
			assertLimitsCoverRequests(t, rec, tt.wantCPU, tt.wantMem)
			if rec.Limits.CPU != nil && rec.Limits.CPU == reqCPU {
				t.Error("raised cpu limit aliases the request")
			}
			if rec.Limits.Memory != nil && rec.Limits.Memory == reqMem {
				t.Error("raised memory limit aliases the request")
			}
		})
	}
}

func TestPercentileStrategy_LimitsCoverRequests(t *testing.T) {
	inverted := func(maxChange float64) map[string]interface{} {
		return map[string]interface{}{
			"requestsBufferPercent": 50.0,
			"limitsBufferPercent":   0.0,
			"maxChangePercent":      maxChange,
		}
	}
	flat := func(cpu, mem string) prometheus.ContainerMetrics {
		return prometheus.ContainerMetrics{ContainerName: "app", P95CPU: qty(cpu), P99CPU: qty(cpu), P95Memory: qty(mem), P99Memory: qty(mem)}
	}

	tests := []struct {
		name                         string
		metrics                      prometheus.ContainerMetrics
		current                      autoscalingv1alpha1.ResourceRequirements
		config                       StrategyConfig
		wantReqCPU, wantLimCPU       string
		wantReqMem, wantLimMem       string
		wantRaisedCPU, wantRaisedMem bool
	}{
		{
			// The request is clamped from 1 to 700m by the 30% maximum
			// change; the limit has no current value to clamp against and
			// is only floored to 100m by the safety rail.
			name:    "nil current limit with cpu request clamped down",
			metrics: prometheus.ContainerMetrics{ContainerName: "app", P95CPU: qty("1m"), P99CPU: qty("2m")},
			current: autoscalingv1alpha1.ResourceRequirements{
				Requests: &autoscalingv1alpha1.ResourceValues{CPU: qty("1")},
			},
			config: StrategyConfig{
				Intent:      autoscalingv1alpha1.IntentCritical,
				SafetyRails: SafetyRailsConfig{MinCPU: qty("100m")},
			},
			wantReqCPU:    "700m",
			wantLimCPU:    "700m",
			wantRaisedCPU: true,
		},
		{
			name: "nil current limit with memory request clamped down",
			metrics: prometheus.ContainerMetrics{
				ContainerName: "app",
				P95CPU:        qty("100m"), P99CPU: qty("200m"),
				P95Memory: qty("1Mi"), P99Memory: qty("2Mi"),
			},
			current: autoscalingv1alpha1.ResourceRequirements{
				Requests: &autoscalingv1alpha1.ResourceValues{Memory: qty("1Gi")},
			},
			wantReqCPU:    "120m",
			wantLimCPU:    "260m",
			wantReqMem:    "512Mi",
			wantLimMem:    "512Mi",
			wantRaisedMem: true,
		},
		{
			name:    "nil current request with cpu limit clamped up",
			metrics: prometheus.ContainerMetrics{ContainerName: "app", P95CPU: qty("1"), P99CPU: qty("1100m")},
			current: autoscalingv1alpha1.ResourceRequirements{
				Limits: &autoscalingv1alpha1.ResourceValues{CPU: qty("100m")},
			},
			wantReqCPU:    "1200m",
			wantLimCPU:    "1200m",
			wantRaisedCPU: true,
		},
		{
			name:    "both current values set and still crossing after gradual change",
			metrics: flat("1", "1Gi"),
			current: autoscalingv1alpha1.ResourceRequirements{
				Requests: &autoscalingv1alpha1.ResourceValues{CPU: qty("1"), Memory: qty("1Gi")},
				Limits:   &autoscalingv1alpha1.ResourceValues{CPU: qty("1"), Memory: qty("1Gi")},
			},
			config:        StrategyConfig{Parameters: inverted(30)},
			wantReqCPU:    "1300m",
			wantLimCPU:    "1300m",
			wantReqMem:    "1395864371",
			wantLimMem:    "1395864371",
			wantRaisedCPU: true,
			wantRaisedMem: true,
		},
		{
			// Unclamped, the request (120m) is below the limit (260m);
			// clamping each against its own current value crosses them.
			name:    "crossing created by gradual change against inconsistent current values",
			metrics: sampleMetrics(),
			current: autoscalingv1alpha1.ResourceRequirements{
				Requests: &autoscalingv1alpha1.ResourceValues{CPU: qty("1"), Memory: qty("1Gi")},
				Limits:   &autoscalingv1alpha1.ResourceValues{CPU: qty("600m"), Memory: qty("600Mi")},
			},
			wantReqCPU:    "500m",
			wantLimCPU:    "500m",
			wantReqMem:    "512Mi",
			wantLimMem:    "512Mi",
			wantRaisedCPU: true,
			wantRaisedMem: true,
		},
		{
			name:          "requests buffer above limits buffer",
			metrics:       flat("100m", "100Mi"),
			config:        StrategyConfig{Parameters: inverted(100)},
			wantReqCPU:    "150m",
			wantLimCPU:    "150m",
			wantReqMem:    "150Mi",
			wantLimMem:    "150Mi",
			wantRaisedCPU: true,
			wantRaisedMem: true,
		},
		{
			name:    "requests buffer above limits buffer from parameters overriding an intent",
			metrics: flat("100m", "100Mi"),
			config: StrategyConfig{
				Intent:     autoscalingv1alpha1.IntentEco,
				Parameters: map[string]interface{}{"requestsBufferPercent": 80.0, "limitsBufferPercent": 10.0},
			},
			wantReqCPU:    "180m",
			wantLimCPU:    "180m",
			wantReqMem:    "180Mi",
			wantLimMem:    "180Mi",
			wantRaisedCPU: true,
			wantRaisedMem: true,
		},
		{
			name:    "maximum safety rail caps only the request",
			metrics: flat("1", "1Gi"),
			config: StrategyConfig{
				Parameters:  inverted(100),
				SafetyRails: SafetyRailsConfig{MaxCPU: qty("1200m"), MaxMemory: qty("1200Mi")},
			},
			wantReqCPU:    "1200m",
			wantLimCPU:    "1200m",
			wantReqMem:    "1200Mi",
			wantLimMem:    "1200Mi",
			wantRaisedCPU: true,
			wantRaisedMem: true,
		},
		{
			name:    "minimum safety rail lifts only the limit",
			metrics: flat("100m", "100Mi"),
			config: StrategyConfig{
				Parameters:  inverted(100),
				SafetyRails: SafetyRailsConfig{MinCPU: qty("120m"), MinMemory: qty("120Mi")},
			},
			wantReqCPU:    "150m",
			wantLimCPU:    "150m",
			wantReqMem:    "150Mi",
			wantLimMem:    "150Mi",
			wantRaisedCPU: true,
			wantRaisedMem: true,
		},
		{
			name: "quota caps only the request",
			metrics: prometheus.ContainerMetrics{
				ContainerName: "app",
				P95CPU:        qty("1"), P99CPU: qty("1100m"),
				P95Memory: qty("1Gi"), P99Memory: qty("1100Mi"),
			},
			config: StrategyConfig{
				Parameters:    map[string]interface{}{"requestsBufferPercent": 100.0, "limitsBufferPercent": 0.0},
				ResourceQuota: &ResourceQuotaConfig{CPULimit: qty("1500m"), MemoryLimit: qty("1500Mi")},
			},
			wantReqCPU:    "1500m",
			wantLimCPU:    "1500m",
			wantReqMem:    "1500Mi",
			wantLimMem:    "1500Mi",
			wantRaisedCPU: true,
			wantRaisedMem: true,
		},
		{
			name:    "quota caps request and limit to the same value",
			metrics: flat("4", "4Gi"),
			config: StrategyConfig{
				Parameters:    inverted(100),
				ResourceQuota: &ResourceQuotaConfig{CPULimit: qty("3"), MemoryLimit: qty("3Gi")},
			},
			wantReqCPU: "3",
			wantLimCPU: "3",
			wantReqMem: "3Gi",
			wantLimMem: "3Gi",
		},
		{
			name:       "missing limit metrics do not invent limits",
			metrics:    prometheus.ContainerMetrics{ContainerName: "app", P95CPU: qty("1m"), P95Memory: qty("1Mi")},
			current:    autoscalingv1alpha1.ResourceRequirements{Requests: &autoscalingv1alpha1.ResourceValues{CPU: qty("1"), Memory: qty("1Gi")}},
			wantReqCPU: "500m",
			wantReqMem: "512Mi",
		},
		{
			name:       "missing request metrics leave limits as computed",
			metrics:    prometheus.ContainerMetrics{ContainerName: "app", P99CPU: qty("1m"), P99Memory: qty("1Mi")},
			config:     StrategyConfig{SafetyRails: SafetyRailsConfig{MinCPU: qty("100m")}},
			wantLimCPU: "100m",
			wantLimMem: "1363148",
		},
		{
			name:    "equal request and limit are left alone",
			metrics: flat("100m", "100Mi"),
			config: StrategyConfig{Parameters: map[string]interface{}{
				"requestsBufferPercent": 0.0, "limitsBufferPercent": 0.0,
			}},
			wantReqCPU: "100m",
			wantLimCPU: "100m",
			wantReqMem: "100Mi",
			wantLimMem: "100Mi",
		},
		{
			name:    "only memory crosses",
			metrics: prometheus.ContainerMetrics{ContainerName: "app", P95CPU: qty("100m"), P99CPU: qty("200m"), P95Memory: qty("200Mi"), P99Memory: qty("100Mi")},
			config: StrategyConfig{Parameters: map[string]interface{}{
				"requestsBufferPercent": 0.0, "limitsBufferPercent": 0.0,
			}},
			wantReqCPU:    "100m",
			wantLimCPU:    "200m",
			wantReqMem:    "200Mi",
			wantLimMem:    "200Mi",
			wantRaisedMem: true,
		},
	}

	s := NewPercentileStrategy()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			current := map[string]autoscalingv1alpha1.ResourceRequirements{"app": tt.current}
			recs, err := s.Calculate(context.Background(), []prometheus.ContainerMetrics{tt.metrics}, current, tt.config)
			if err != nil {
				t.Fatalf("Calculate: %v", err)
			}
			rec := recs[0]
			assertQty(t, "requests.cpu", rec.Requests.CPU, tt.wantReqCPU)
			assertQty(t, "limits.cpu", rec.Limits.CPU, tt.wantLimCPU)
			assertQty(t, "requests.memory", rec.Requests.Memory, tt.wantReqMem)
			assertQty(t, "limits.memory", rec.Limits.Memory, tt.wantLimMem)
			assertLimitsCoverRequests(t, rec, tt.wantRaisedCPU, tt.wantRaisedMem)
		})
	}
}

func TestDSPStrategy_LimitsCoverRequests(t *testing.T) {
	tests := []struct {
		name string
		// sampleInterval selects the path: at one minute the default daily
		// period does not fit the synthetic history and the strategy falls
		// back to P95/P99; at one hour it uses the DSP prediction.
		sampleInterval               time.Duration
		metrics                      prometheus.ContainerMetrics
		config                       StrategyConfig
		wantReqCPU, wantLimCPU       string
		wantReqMem, wantLimMem       string
		wantRaisedCPU, wantRaisedMem bool
	}{
		{
			name:           "fallback with the minimum safety rail lifting only the request",
			sampleInterval: time.Minute,
			metrics: prometheus.ContainerMetrics{
				ContainerName: "app",
				P95CPU:        qty("50m"), P99CPU: qty("100m"),
				P95Memory: qty("50Mi"), P99Memory: qty("100Mi"),
			},
			config:        StrategyConfig{SafetyRails: SafetyRailsConfig{MinCPU: qty("200m"), MinMemory: qty("200Mi")}},
			wantReqCPU:    "200m",
			wantLimCPU:    "200m",
			wantReqMem:    "200Mi",
			wantLimMem:    "200Mi",
			wantRaisedCPU: true,
			wantRaisedMem: true,
		},
		{
			name:           "fallback with P95 above P99",
			sampleInterval: time.Minute,
			metrics: prometheus.ContainerMetrics{
				ContainerName: "app",
				P95CPU:        qty("300m"), P99CPU: qty("200m"),
				P95Memory: qty("100Mi"), P99Memory: qty("200Mi"),
			},
			wantReqCPU:    "300m",
			wantLimCPU:    "300m",
			wantReqMem:    "100Mi",
			wantLimMem:    "200Mi",
			wantRaisedCPU: true,
		},
		{
			name:           "fallback with quota capping request and limit",
			sampleInterval: time.Minute,
			metrics: prometheus.ContainerMetrics{
				ContainerName: "app",
				P95CPU:        qty("3"), P99CPU: qty("2"),
				P95Memory: qty("3Gi"), P99Memory: qty("2Gi"),
			},
			config:        StrategyConfig{ResourceQuota: &ResourceQuotaConfig{CPULimit: qty("2500m"), MemoryLimit: qty("1Gi")}},
			wantReqCPU:    "2500m",
			wantLimCPU:    "2500m",
			wantReqMem:    "1Gi",
			wantLimMem:    "1Gi",
			wantRaisedCPU: true,
		},
		{
			name:           "fallback without P99 does not invent a limit",
			sampleInterval: time.Minute,
			metrics:        prometheus.ContainerMetrics{ContainerName: "app", P95CPU: qty("100m"), P95Memory: qty("100Mi")},
			config:         StrategyConfig{SafetyRails: SafetyRailsConfig{MinCPU: qty("200m")}},
			wantReqCPU:     "200m",
			wantReqMem:     "100Mi",
		},
		{
			name:           "fallback with equal values",
			sampleInterval: time.Minute,
			metrics: prometheus.ContainerMetrics{
				ContainerName: "app",
				P95CPU:        qty("100m"), P99CPU: qty("100m"),
				P95Memory: qty("100Mi"), P99Memory: qty("100Mi"),
			},
			wantReqCPU: "100m",
			wantLimCPU: "100m",
			wantReqMem: "100Mi",
			wantLimMem: "100Mi",
		},
		{
			name:           "prediction with the minimum safety rail lifting only the request",
			sampleInterval: time.Hour,
			metrics:        sampleMetrics(),
			config:         StrategyConfig{SafetyRails: SafetyRailsConfig{MinCPU: qty("2"), MinMemory: qty("4Gi")}},
			wantReqCPU:     "2",
			wantLimCPU:     "2",
			wantReqMem:     "4Gi",
			wantLimMem:     "4Gi",
			wantRaisedCPU:  true,
			wantRaisedMem:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recs, err := NewDSPStrategy(tt.sampleInterval).Calculate(context.Background(), []prometheus.ContainerMetrics{tt.metrics}, nil, tt.config)
			if err != nil {
				t.Fatalf("Calculate: %v", err)
			}
			rec := recs[0]
			_, predicted := rec.Metadata["cpu_period"]
			if wantPredicted := tt.sampleInterval == time.Hour; predicted != wantPredicted {
				t.Fatalf("cpu prediction used = %v, want %v (metadata %v)", predicted, wantPredicted, rec.Metadata)
			}
			assertQty(t, "requests.cpu", rec.Requests.CPU, tt.wantReqCPU)
			assertQty(t, "limits.cpu", rec.Limits.CPU, tt.wantLimCPU)
			assertQty(t, "requests.memory", rec.Requests.Memory, tt.wantReqMem)
			assertQty(t, "limits.memory", rec.Limits.Memory, tt.wantLimMem)
			assertLimitsCoverRequests(t, rec, tt.wantRaisedCPU, tt.wantRaisedMem)
		})
	}

	t.Run("fallback does not modify the input metrics", func(t *testing.T) {
		m := prometheus.ContainerMetrics{ContainerName: "app", P95CPU: qty("300m"), P99CPU: qty("200m")}
		if _, err := NewDSPStrategy(time.Minute).Calculate(context.Background(), []prometheus.ContainerMetrics{m}, nil, StrategyConfig{}); err != nil {
			t.Fatal(err)
		}
		assertQty(t, "P95CPU", m.P95CPU, "300m")
		assertQty(t, "P99CPU", m.P99CPU, "200m")
	})
}

func TestARIMAStrategy_LimitsCoverRequests(t *testing.T) {
	forecast := historyMetrics(dailySeries, scaled(dailySeries, 512*mib))

	t.Run("forecast with nil current limits and requests clamped down", func(t *testing.T) {
		rec := calculateARIMA(t, forecast, autoscalingv1alpha1.ResourceRequirements{
			Requests: &autoscalingv1alpha1.ResourceValues{CPU: qty("10"), Memory: qty("10Gi")},
		}, StrategyConfig{})
		if _, ok := rec.Metadata["cpu_model"]; !ok {
			t.Fatalf("cpu should be forecast: %v", rec.Metadata)
		}
		assertQty(t, "requests.cpu", rec.Requests.CPU, "5")
		assertQty(t, "limits.cpu", rec.Limits.CPU, "5")
		assertQty(t, "requests.memory", rec.Requests.Memory, "5Gi")
		assertQty(t, "limits.memory", rec.Limits.Memory, "5Gi")
		assertLimitsCoverRequests(t, rec, true, true)
	})

	t.Run("forecast with requests buffer above limits buffer", func(t *testing.T) {
		rec := calculateARIMA(t, forecast, autoscalingv1alpha1.ResourceRequirements{}, StrategyConfig{Parameters: map[string]interface{}{
			"confidence": 0.95, "limitsConfidence": 0.95,
			"requestsBufferPercent": 50.0, "limitsBufferPercent": 0.0,
		}})
		if _, ok := rec.Metadata["cpu_model"]; !ok {
			t.Fatalf("cpu should be forecast: %v", rec.Metadata)
		}
		if rec.Limits.CPU.Cmp(*rec.Requests.CPU) != 0 || rec.Limits.Memory.Cmp(*rec.Requests.Memory) != 0 {
			t.Errorf("limits %s/%s should be raised to requests %s/%s",
				rec.Limits.CPU, rec.Limits.Memory, rec.Requests.CPU, rec.Requests.Memory)
		}
		assertLimitsCoverRequests(t, rec, true, true)
	})

	t.Run("forecast without crossing is left alone", func(t *testing.T) {
		rec := calculateARIMA(t, forecast, autoscalingv1alpha1.ResourceRequirements{}, StrategyConfig{})
		assertLimitsCoverRequests(t, rec, false, false)
	})

	t.Run("percentile fallback with nil current limits and requests clamped down", func(t *testing.T) {
		rec := calculateARIMA(t, historyMetrics(nil, nil), autoscalingv1alpha1.ResourceRequirements{
			Requests: &autoscalingv1alpha1.ResourceValues{CPU: qty("1"), Memory: qty("1Gi")},
		}, StrategyConfig{})
		for _, key := range []string{"cpu_fallback", "memory_fallback"} {
			if _, ok := rec.Metadata[key]; !ok {
				t.Errorf("%s missing: %v", key, rec.Metadata)
			}
		}
		assertQty(t, "requests.cpu", rec.Requests.CPU, "500m")
		assertQty(t, "limits.cpu", rec.Limits.CPU, "500m")
		assertQty(t, "requests.memory", rec.Requests.Memory, "512Mi")
		assertQty(t, "limits.memory", rec.Limits.Memory, "512Mi")
		assertLimitsCoverRequests(t, rec, true, true)
	})
}

// randomInputs generates strategy inputs that span several orders of
// magnitude around the clamp points, with missing and zero values.
type randomInputs struct {
	r *rand.Rand
}

func (g randomInputs) maybe(f func() *resource.Quantity) *resource.Quantity {
	if g.r.Intn(4) == 0 {
		return nil
	}
	return f()
}

func (g randomInputs) cpu() *resource.Quantity {
	if g.r.Intn(10) == 0 {
		return resource.NewMilliQuantity(0, resource.DecimalSI)
	}
	return resource.NewMilliQuantity((1+g.r.Int63n(9))<<uint(g.r.Intn(17)), resource.DecimalSI)
}

func (g randomInputs) memory() *resource.Quantity {
	if g.r.Intn(10) == 0 {
		return resource.NewQuantity(0, resource.BinarySI)
	}
	return resource.NewQuantity((1+g.r.Int63n(9))<<uint(10+g.r.Intn(26)), resource.BinarySI)
}

func (g randomInputs) values() *autoscalingv1alpha1.ResourceValues {
	if g.r.Intn(3) == 0 {
		return nil
	}
	return &autoscalingv1alpha1.ResourceValues{CPU: g.maybe(g.cpu), Memory: g.maybe(g.memory)}
}

func (g randomInputs) metrics() prometheus.ContainerMetrics {
	return prometheus.ContainerMetrics{
		ContainerName: "app",
		P95CPU:        g.maybe(g.cpu), P99CPU: g.maybe(g.cpu),
		P95Memory: g.maybe(g.memory), P99Memory: g.maybe(g.memory),
	}
}

func (g randomInputs) config() StrategyConfig {
	intents := []autoscalingv1alpha1.Intent{"", autoscalingv1alpha1.IntentEco, autoscalingv1alpha1.IntentBalanced, autoscalingv1alpha1.IntentCritical}
	params := map[string]interface{}{}
	for _, key := range []string{paramRequestsBufferPercent, paramLimitsBufferPercent, paramMaxChangePercent} {
		if g.r.Intn(2) == 0 {
			params[key] = float64(g.r.Intn(201))
		}
	}
	config := StrategyConfig{
		Intent:     intents[g.r.Intn(len(intents))],
		Parameters: params,
		SafetyRails: SafetyRailsConfig{
			MinCPU: g.maybe(g.cpu), MaxCPU: g.maybe(g.cpu),
			MinMemory: g.maybe(g.memory), MaxMemory: g.maybe(g.memory),
		},
	}
	if g.r.Intn(2) == 0 {
		config.ResourceQuota = &ResourceQuotaConfig{CPULimit: g.maybe(g.cpu), MemoryLimit: g.maybe(g.memory)}
	}
	return config
}

// TestStrategies_LimitsNeverBelowRequests searches random inputs, including
// missing values, zero values, inverted buffers, inverted percentiles,
// misconfigured safety rails and small quotas, for a recommendation whose
// limit is below its request.
func TestStrategies_LimitsNeverBelowRequests(t *testing.T) {
	g := randomInputs{r: rand.New(rand.NewSource(42))}
	// DSP runs an FFT per resource, so it only checks every 50th input.
	strategies := []struct {
		s     Strategy
		every int
	}{
		{NewPercentileStrategy(), 1},
		{NewARIMAStrategy(5 * time.Minute), 1},
		{NewDSPStrategy(time.Minute), 50},
		{NewDSPStrategy(time.Hour), 50},
	}
	const iterations = 3000
	for i := 0; i < iterations; i++ {
		m := g.metrics()
		current := autoscalingv1alpha1.ResourceRequirements{Requests: g.values(), Limits: g.values()}
		config := g.config()
		for _, st := range strategies {
			if i%st.every != 0 {
				continue
			}
			recs, err := st.s.Calculate(context.Background(), []prometheus.ContainerMetrics{m},
				map[string]autoscalingv1alpha1.ResourceRequirements{"app": current}, config)
			if err != nil {
				t.Fatalf("iteration %d %s: Calculate: %v", i, st.s.Name(), err)
			}
			if msg := checkLimitsInvariant(st.s, m, config, recs[0]); msg != "" {
				t.Fatalf("iteration %d %s: %s; metrics %+v current %+v config %+v rec %+v",
					i, st.s.Name(), msg, m, current, config, recs[0])
			}
		}
	}
}

// checkLimitsInvariant returns a description of the first violated
// property of rec, or "" if there is none.
func checkLimitsInvariant(s Strategy, m prometheus.ContainerMetrics, config StrategyConfig, rec Recommendation) string {
	_, isDSP := s.(*DSPStrategy)
	for _, v := range []struct {
		name           string
		request, limit *resource.Quantity
		key            string
		p95, p99       *resource.Quantity
		maxRail, quota *resource.Quantity
	}{
		{"cpu", rec.Requests.CPU, rec.Limits.CPU, MetadataCPULimitRaisedToRequest, m.P95CPU, m.P99CPU, config.SafetyRails.MaxCPU, quotaCPU(config.ResourceQuota)},
		{"memory", rec.Requests.Memory, rec.Limits.Memory, MetadataMemoryLimitRaisedToRequest, m.P95Memory, m.P99Memory, config.SafetyRails.MaxMemory, quotaMemory(config.ResourceQuota)},
	} {
		if v.request != nil && v.limit != nil && v.limit.Cmp(*v.request) < 0 {
			return fmt.Sprintf("limits.%s %s < requests.%s %s", v.name, v.limit, v.name, v.request)
		}
		if rec.Metadata[v.key] == true && (v.request == nil || v.limit == nil || v.limit.Cmp(*v.request) != 0) {
			return fmt.Sprintf("%s set but limit %v != request %v", v.key, v.limit, v.request)
		}
		if isDSP {
			if v.p95 == nil && (v.request != nil || v.limit != nil) {
				return fmt.Sprintf("%s values invented without P95", v.name)
			}
			continue
		}
		// Percentile and ARIMA (falling back without history) derive
		// requests from P95 and limits from P99 only, and cap both at the
		// maximum safety rail and the quota.
		if (v.request == nil) != (v.p95 == nil) || (v.limit == nil) != (v.p99 == nil) {
			return fmt.Sprintf("%s values do not match the available percentiles", v.name)
		}
		for _, bound := range []*resource.Quantity{v.maxRail, v.quota} {
			for _, q := range []*resource.Quantity{v.request, v.limit} {
				if bound != nil && q != nil && q.Cmp(*bound) > 0 {
					return fmt.Sprintf("%s %s exceeds the bound %s", v.name, q, bound)
				}
			}
		}
	}
	return ""
}

func quotaCPU(q *ResourceQuotaConfig) *resource.Quantity {
	if q == nil {
		return nil
	}
	return q.CPULimit
}

func quotaMemory(q *ResourceQuotaConfig) *resource.Quantity {
	if q == nil {
		return nil
	}
	return q.MemoryLimit
}
