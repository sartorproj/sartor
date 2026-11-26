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
	"sort"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/metrics/prometheus"
)

func qty(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

func assertQty(t *testing.T, what string, got *resource.Quantity, want string) {
	t.Helper()
	if want == "" {
		if got != nil {
			t.Errorf("%s = %s, want nil", what, got.String())
		}
		return
	}
	if got == nil {
		t.Errorf("%s = nil, want %s", what, want)
		return
	}
	if got.Cmp(resource.MustParse(want)) != 0 {
		t.Errorf("%s = %s, want %s", what, got.String(), want)
	}
}

func sampleMetrics() prometheus.ContainerMetrics {
	return prometheus.ContainerMetrics{
		ContainerName: "app",
		P95CPU:        resource.NewMilliQuantity(100, resource.DecimalSI),
		P99CPU:        resource.NewMilliQuantity(200, resource.DecimalSI),
		P95Memory:     resource.NewQuantity(100*1024*1024, resource.BinarySI),
		P99Memory:     resource.NewQuantity(200*1024*1024, resource.BinarySI),
	}
}

func TestPercentileStrategy_Calculate(t *testing.T) {
	tests := []struct {
		name        string
		metrics     prometheus.ContainerMetrics
		current     autoscalingv1alpha1.ResourceRequirements
		config      StrategyConfig
		wantReqCPU  string
		wantLimCPU  string
		wantReqMem  string
		wantLimMem  string
		wantCapped  bool
		wantReason  string
		wantBufMeta float64
	}{
		{
			name:        "balanced default buffers without current resources",
			metrics:     sampleMetrics(),
			wantReqCPU:  "120m",
			wantLimCPU:  "260m",
			wantReqMem:  "120Mi",
			wantLimMem:  "260Mi",
			wantBufMeta: 20,
		},
		{
			name:        "eco intent uses smaller buffers and no change limit",
			metrics:     sampleMetrics(),
			config:      StrategyConfig{Intent: autoscalingv1alpha1.IntentEco},
			current:     autoscalingv1alpha1.ResourceRequirements{Requests: &autoscalingv1alpha1.ResourceValues{CPU: qty("10m")}},
			wantReqCPU:  "110m",
			wantLimCPU:  "240m",
			wantReqMem:  "110Mi",
			wantLimMem:  "240Mi",
			wantBufMeta: 10,
		},
		{
			name:    "critical intent limits increases to 30 percent",
			metrics: sampleMetrics(),
			config:  StrategyConfig{Intent: autoscalingv1alpha1.IntentCritical},
			current: autoscalingv1alpha1.ResourceRequirements{
				Requests: &autoscalingv1alpha1.ResourceValues{CPU: qty("50m"), Memory: qty("50Mi")},
				Limits:   &autoscalingv1alpha1.ResourceValues{CPU: qty("100m"), Memory: qty("100Mi")},
			},
			wantReqCPU:  "65m",
			wantLimCPU:  "130m",
			wantReqMem:  "65Mi",
			wantLimMem:  "130Mi",
			wantBufMeta: 40,
		},
		{
			name:    "large decreases are limited too",
			metrics: sampleMetrics(),
			current: autoscalingv1alpha1.ResourceRequirements{
				Requests: &autoscalingv1alpha1.ResourceValues{CPU: qty("1"), Memory: qty("1Gi")},
				Limits:   &autoscalingv1alpha1.ResourceValues{CPU: qty("2"), Memory: qty("2Gi")},
			},
			wantReqCPU:  "500m",
			wantLimCPU:  "1",
			wantReqMem:  "512Mi",
			wantLimMem:  "1Gi",
			wantBufMeta: 20,
		},
		{
			name:    "zero current value does not limit the change",
			metrics: sampleMetrics(),
			current: autoscalingv1alpha1.ResourceRequirements{
				Requests: &autoscalingv1alpha1.ResourceValues{CPU: qty("0")},
			},
			wantReqCPU:  "120m",
			wantLimCPU:  "260m",
			wantReqMem:  "120Mi",
			wantLimMem:  "260Mi",
			wantBufMeta: 20,
		},
		{
			name:    "parameters override the default buffers",
			metrics: sampleMetrics(),
			config: StrategyConfig{Parameters: map[string]interface{}{
				"requestsBufferPercent": 0.0,
				"limitsBufferPercent":   50.0,
				"maxChangePercent":      100.0,
			}},
			wantReqCPU:  "100m",
			wantLimCPU:  "300m",
			wantReqMem:  "100Mi",
			wantLimMem:  "300Mi",
			wantBufMeta: 0,
		},
		{
			name:    "parameters override the intent buffers",
			metrics: sampleMetrics(),
			config: StrategyConfig{
				Intent: autoscalingv1alpha1.IntentCritical,
				Parameters: map[string]interface{}{
					"requestsBufferPercent": 0.0,
					"limitsBufferPercent":   0.0,
					"maxChangePercent":      100.0,
				},
			},
			wantReqCPU:  "100m",
			wantLimCPU:  "200m",
			wantReqMem:  "100Mi",
			wantLimMem:  "200Mi",
			wantBufMeta: 0,
		},
		{
			name:    "minimum safety rail",
			metrics: sampleMetrics(),
			config: StrategyConfig{SafetyRails: SafetyRailsConfig{
				MinCPU:    qty("500m"),
				MinMemory: qty("512Mi"),
			}},
			wantReqCPU:  "500m",
			wantLimCPU:  "500m",
			wantReqMem:  "512Mi",
			wantLimMem:  "512Mi",
			wantCapped:  true,
			wantReason:  "capped at minimum safety rail",
			wantBufMeta: 20,
		},
		{
			name:    "maximum safety rail",
			metrics: sampleMetrics(),
			config: StrategyConfig{SafetyRails: SafetyRailsConfig{
				MaxCPU:    qty("150m"),
				MaxMemory: qty("150Mi"),
			}},
			wantReqCPU:  "120m",
			wantLimCPU:  "150m",
			wantReqMem:  "120Mi",
			wantLimMem:  "150Mi",
			wantCapped:  true,
			wantReason:  "capped at maximum safety rail",
			wantBufMeta: 20,
		},
		{
			name:    "cpu quota",
			metrics: sampleMetrics(),
			config: StrategyConfig{ResourceQuota: &ResourceQuotaConfig{
				CPULimit: qty("100m"),
			}},
			wantReqCPU:  "100m",
			wantLimCPU:  "100m",
			wantReqMem:  "120Mi",
			wantLimMem:  "260Mi",
			wantCapped:  true,
			wantReason:  cappedAtQuotaCPU,
			wantBufMeta: 20,
		},
		{
			name:    "memory quota",
			metrics: sampleMetrics(),
			config: StrategyConfig{ResourceQuota: &ResourceQuotaConfig{
				CPULimit:    qty("10"),
				MemoryLimit: qty("100Mi"),
			}},
			wantReqCPU:  "120m",
			wantLimCPU:  "260m",
			wantReqMem:  "100Mi",
			wantLimMem:  "100Mi",
			wantCapped:  true,
			wantReason:  cappedAtQuotaMemory,
			wantBufMeta: 20,
		},
		{
			name:    "missing memory metrics leave memory unset",
			metrics: prometheus.ContainerMetrics{ContainerName: "app", P95CPU: qty("100m"), P99CPU: qty("200m")},
			config: StrategyConfig{ResourceQuota: &ResourceQuotaConfig{
				MemoryLimit: qty("1Mi"),
			}},
			wantReqCPU:  "120m",
			wantLimCPU:  "260m",
			wantBufMeta: 20,
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
			if len(recs) != 1 {
				t.Fatalf("got %d recommendations, want 1", len(recs))
			}
			rec := recs[0]
			if rec.ContainerName != "app" || rec.Method != StrategyNamePercentile {
				t.Errorf("container/method = %s/%s", rec.ContainerName, rec.Method)
			}
			assertQty(t, "requests.cpu", rec.Requests.CPU, tt.wantReqCPU)
			assertQty(t, "limits.cpu", rec.Limits.CPU, tt.wantLimCPU)
			assertQty(t, "requests.memory", rec.Requests.Memory, tt.wantReqMem)
			assertQty(t, "limits.memory", rec.Limits.Memory, tt.wantLimMem)
			if rec.Capped != tt.wantCapped || rec.CappedReason != tt.wantReason {
				t.Errorf("capped = %v %q, want %v %q", rec.Capped, rec.CappedReason, tt.wantCapped, tt.wantReason)
			}
			if got := rec.Metadata["requests_buffer_percent"]; got != tt.wantBufMeta {
				t.Errorf("requests_buffer_percent = %v, want %v", got, tt.wantBufMeta)
			}
		})
	}
}

func TestApplyBuffer_Nil(t *testing.T) {
	if applyBuffer(nil, 10) != nil {
		t.Error("applyBuffer(nil) should be nil")
	}
	s := NewPercentileStrategy()
	if s.applySafetyRails(nil, qty("1"), qty("2"), &Recommendation{}) != nil {
		t.Error("applySafetyRails(nil) should be nil")
	}
	rec := Recommendation{Requests: ResourceValues{CPU: qty("1")}}
	s.applyQuotaLimits(&rec, nil)
	if rec.Capped {
		t.Error("nil quota must not cap")
	}
}

func TestPercentileStrategy_ValidateFitProfileParameters(t *testing.T) {
	tests := []struct {
		name    string
		params  map[string]interface{}
		wantErr bool
	}{
		{name: "nil", params: nil},
		{name: "valid", params: map[string]interface{}{"requestsBufferPercent": 20.0, "limitsBufferPercent": 30.0, "maxChangePercent": 50.0}},
		{name: "bounds", params: map[string]interface{}{"requestsBufferPercent": 0.0, "limitsBufferPercent": 1000.0}},
		{name: "negative requests buffer", params: map[string]interface{}{"requestsBufferPercent": -1.0}, wantErr: true},
		{name: "too large limits buffer", params: map[string]interface{}{"limitsBufferPercent": 1001.0}, wantErr: true},
		{name: "negative max change", params: map[string]interface{}{"maxChangePercent": -5.0}, wantErr: true},
		{name: "non-number values are ignored", params: map[string]interface{}{"requestsBufferPercent": "lots"}},
	}
	s := NewPercentileStrategy()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := s.ValidateFitProfileParameters(tt.params); (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
	if err := s.ValidateConfig(StrategyConfig{}); err != nil {
		t.Errorf("ValidateConfig: %v", err)
	}
}

func TestPercentileStrategy_DefaultSpecAndChartLines(t *testing.T) {
	s := NewPercentileStrategy()
	spec := s.GetDefaultFitProfileSpec()
	if spec.Strategy != StrategyNamePercentile || spec.ParametersSchema["type"] != "object" {
		t.Errorf("unexpected spec: %+v", spec)
	}
	if err := s.ValidateFitProfileParameters(spec.ExampleParameters); err != nil {
		t.Errorf("example parameters must be valid: %v", err)
	}

	metrics := []prometheus.ContainerMetrics{sampleMetrics(), {ContainerName: "other", P95CPU: qty("1")}}
	recs := []Recommendation{{ContainerName: "app"}}
	lines, err := s.GetChartLines(context.Background(), metrics, nil, recs, StrategyConfig{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{
		"cpu_p95_requests":    0.1,
		"cpu_p99_limits":      0.2,
		"memory_p95_requests": 100 * 1024 * 1024,
		"memory_p99_limits":   200 * 1024 * 1024,
	}
	if len(lines) != len(want) {
		t.Fatalf("lines = %v, want keys %v", lines, want)
	}
	for name, v := range want {
		if len(lines[name]) != 1 {
			t.Fatalf("line %s has %d points, want 1", name, len(lines[name]))
		}
		for _, got := range lines[name] {
			if got != v {
				t.Errorf("line %s = %v, want %v", name, got, v)
			}
		}
	}
}

func TestStrategyRegistry(t *testing.T) {
	r := NewStrategyRegistry()
	r.Register(NewPercentileStrategy())
	r.Register(NewDSPStrategy(0))

	names := r.List()
	sort.Strings(names)
	if len(names) != 2 || names[0] != StrategyNameDSP || names[1] != StrategyNamePercentile {
		t.Errorf("List() = %v", names)
	}
	if s, ok := r.Get(StrategyNamePercentile); !ok || s.Name() != StrategyNamePercentile {
		t.Errorf("Get(percentile) = %v, %v", s, ok)
	}
	if _, ok := r.Get("missing"); ok {
		t.Error("Get(missing) should not exist")
	}
}
