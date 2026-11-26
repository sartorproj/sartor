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
	"testing"
	"time"

	"github.com/sartorproj/sartor/internal/metrics/prometheus"
	"github.com/sartorproj/sartor/internal/recommender/preprocessing"
)

func TestDSPStrategy_Calculate(t *testing.T) {
	tests := []struct {
		name    string
		metrics prometheus.ContainerMetrics
		config  StrategyConfig
		check   func(t *testing.T, rec Recommendation)
	}{
		{
			name:    "fft with all preprocessing enabled",
			metrics: sampleMetrics(),
			config: StrategyConfig{
				EnablePeakDetection:  true,
				EnableNormalization:  true,
				EnableOutlierRemoval: true,
			},
			check: func(t *testing.T, rec Recommendation) {
				if _, ok := rec.Metadata["cpu_peaks"]; !ok {
					t.Error("peak detection metadata missing")
				}
				if rec.Requests.CPU == nil || rec.Limits.CPU == nil || rec.Requests.Memory == nil || rec.Limits.Memory == nil {
					t.Errorf("all resources should be set: %+v", rec)
				}
			},
		},
		{
			name:    "max value method",
			metrics: sampleMetrics(),
			config:  StrategyConfig{Parameters: map[string]interface{}{"method": methodMaxValue, "marginFraction": 0.1}},
			check: func(t *testing.T, rec Recommendation) {
				if rec.Requests.CPU == nil || rec.Limits.Memory == nil {
					t.Errorf("resources should be set: %+v", rec)
				}
				if rec.Limits.CPU.Cmp(*rec.Requests.CPU) < 0 {
					t.Errorf("limits.cpu %s < requests.cpu %s", rec.Limits.CPU, rec.Requests.CPU)
				}
			},
		},
		{
			name:    "safety rails cap the result",
			metrics: sampleMetrics(),
			config: StrategyConfig{SafetyRails: SafetyRailsConfig{
				MinCPU:    qty("2"),
				MaxCPU:    qty("1m"),
				MinMemory: qty("4Gi"),
				MaxMemory: qty("1Ki"),
			}},
			check: func(t *testing.T, rec Recommendation) {
				assertQty(t, "requests.cpu", rec.Requests.CPU, "2")
				assertQty(t, "limits.cpu", rec.Limits.CPU, "1m")
				assertQty(t, "requests.memory", rec.Requests.Memory, "4Gi")
				assertQty(t, "limits.memory", rec.Limits.Memory, "1Ki")
				if !rec.Capped || rec.CappedReason != "capped at maximum memory safety rail" {
					t.Errorf("capped = %v %q", rec.Capped, rec.CappedReason)
				}
			},
		},
		{
			name:    "quota caps requests and limits",
			metrics: sampleMetrics(),
			config: StrategyConfig{ResourceQuota: &ResourceQuotaConfig{
				CPULimit:    qty("1m"),
				MemoryLimit: qty("1Ki"),
			}},
			check: func(t *testing.T, rec Recommendation) {
				assertQty(t, "requests.cpu", rec.Requests.CPU, "1m")
				assertQty(t, "limits.cpu", rec.Limits.CPU, "1m")
				assertQty(t, "requests.memory", rec.Requests.Memory, "1Ki")
				assertQty(t, "limits.memory", rec.Limits.Memory, "1Ki")
				if rec.CappedReason != cappedAtQuotaMemory {
					t.Errorf("capped reason = %q", rec.CappedReason)
				}
			},
		},
		{
			name:    "no metrics yields an empty recommendation",
			metrics: prometheus.ContainerMetrics{ContainerName: "app"},
			check: func(t *testing.T, rec Recommendation) {
				if rec.Requests.CPU != nil || rec.Requests.Memory != nil {
					t.Errorf("expected no resources: %+v", rec)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewDSPStrategy(time.Hour)
			recs, err := s.Calculate(context.Background(), []prometheus.ContainerMetrics{tt.metrics}, nil, tt.config)
			if err != nil {
				t.Fatalf("Calculate: %v", err)
			}
			if len(recs) != 1 || recs[0].ContainerName != "app" || recs[0].Method != StrategyNameDSP {
				t.Fatalf("unexpected recommendations: %+v", recs)
			}
			tt.check(t, recs[0])
		})
	}
}

func TestDSPStrategy_GetDSPParameters(t *testing.T) {
	s := NewDSPStrategy(time.Hour)
	defaults := s.getDSPParameters(StrategyConfig{})
	if defaults.Period != 86400 || defaults.MinNumOfSpectrumItems != 10 || defaults.MaxNumOfSpectrumItems != 20 {
		t.Errorf("defaults = %+v", defaults)
	}
	got := s.getDSPParameters(StrategyConfig{Parameters: map[string]interface{}{
		"period":                 int64(3600),
		"marginFraction":         0.5,
		"lowAmplitudeThreshold":  2.0,
		"highFrequencyThreshold": 0.1,
		"minNumOfSpectrumItems":  3,
		"maxNumOfSpectrumItems":  7,
	}})
	if got.Period != 3600 || got.MarginFraction != 0.5 || got.LowAmplitudeThreshold != 2.0 ||
		got.HighFrequencyThreshold != 0.1 || got.MinNumOfSpectrumItems != 3 || got.MaxNumOfSpectrumItems != 7 {
		t.Errorf("overrides = %+v", got)
	}
}

func TestDSPStrategy_PredictResourceEmpty(t *testing.T) {
	s := NewDSPStrategy(time.Hour)
	if _, _, err := s.predictResource(nil, s.getDSPParameters(StrategyConfig{}), resourceTypeCPU, StrategyConfig{}); err == nil {
		t.Error("expected error for empty series")
	}
}

func TestDSPStrategy_ValidateFitProfileParameters(t *testing.T) {
	tests := []struct {
		name    string
		params  map[string]interface{}
		wantErr bool
	}{
		{name: "nil", params: nil},
		{name: "valid fft", params: map[string]interface{}{"method": methodFFT, "marginFraction": 0.2, "minNumOfSpectrumItems": 5.0, "maxNumOfSpectrumItems": 10.0}},
		{name: "valid max value", params: map[string]interface{}{"method": methodMaxValue}},
		{name: "unknown method", params: map[string]interface{}{"method": "magic"}, wantErr: true},
		{name: "margin too large", params: map[string]interface{}{"marginFraction": 2.5}, wantErr: true},
		{name: "negative amplitude threshold", params: map[string]interface{}{"lowAmplitudeThreshold": -1.0}, wantErr: true},
		{name: "frequency threshold above one", params: map[string]interface{}{"highFrequencyThreshold": 1.5}, wantErr: true},
		{name: "min spectrum items out of range", params: map[string]interface{}{"minNumOfSpectrumItems": 101.0}, wantErr: true},
		{name: "max spectrum items out of range", params: map[string]interface{}{"maxNumOfSpectrumItems": -1.0}, wantErr: true},
		{name: "min greater than max", params: map[string]interface{}{"minNumOfSpectrumItems": 20.0, "maxNumOfSpectrumItems": 10.0}, wantErr: true},
	}
	s := NewDSPStrategy(time.Hour)
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

func TestDSPStrategy_DefaultSpecAndChartLines(t *testing.T) {
	s := NewDSPStrategy(time.Hour)
	spec := s.GetDefaultFitProfileSpec()
	if spec.Strategy != StrategyNameDSP || spec.ParametersSchema == nil {
		t.Errorf("unexpected spec: %+v", spec)
	}
	if err := s.ValidateFitProfileParameters(spec.ExampleParameters); err != nil {
		t.Errorf("example parameters must be valid: %v", err)
	}

	metrics := []prometheus.ContainerMetrics{sampleMetrics()}
	recs := []Recommendation{{ContainerName: "app"}, {ContainerName: "missing"}}
	lines, err := s.GetChartLines(context.Background(), metrics, nil, recs, StrategyConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"cpu_predicted_requests", "cpu_predicted_limits", "cpu_min", "cpu_max",
		"memory_predicted_requests", "memory_predicted_limits", "memory_min", "memory_max",
	} {
		if len(lines[name]) != 1 {
			t.Errorf("line %s has %d points, want 1", name, len(lines[name]))
		}
	}
	for _, v := range lines["cpu_max"] {
		if v != 0.1 {
			t.Errorf("cpu_max = %v, want 0.1", v)
		}
	}
}

func TestGetMinMax(t *testing.T) {
	if lo, hi := getMinMax(nil); lo != 0 || hi != 0 {
		t.Errorf("empty = %v, %v", lo, hi)
	}
	series := []preprocessing.TimeSeries{{Value: 3}, {Value: -1}, {Value: 7}}
	if lo, hi := getMinMax(series); lo != -1 || hi != 7 {
		t.Errorf("getMinMax = %v, %v, want -1, 7", lo, hi)
	}
}
