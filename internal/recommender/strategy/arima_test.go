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
	"math"
	"math/rand"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/metrics/prometheus"
)

const (
	testStart     = int64(1_700_000_000)
	samplesPerDay = 288 // at 5m
	week5m        = 7 * samplesPerDay
	mib           = 1024 * 1024
)

// usage returns n samples taken every 5 minutes.
func usage(n int, seed int64, f func(i int, r *rand.Rand) float64) []prometheus.TimeSeriesPoint {
	r := rand.New(rand.NewSource(seed))
	points := make([]prometheus.TimeSeriesPoint, n)
	for i := range points {
		points[i] = prometheus.TimeSeriesPoint{Timestamp: testStart + int64(i)*300, Value: f(i, r)}
	}
	return points
}

func dailyWave(i int) float64 {
	return math.Sin(2 * math.Pi * float64(i) / samplesPerDay)
}

func scaled(points []prometheus.TimeSeriesPoint, factor float64) []prometheus.TimeSeriesPoint {
	out := make([]prometheus.TimeSeriesPoint, len(points))
	for i, p := range points {
		out[i] = prometheus.TimeSeriesPoint{Timestamp: p.Timestamp, Value: p.Value * factor}
	}
	return out
}

func peakOf(points []prometheus.TimeSeriesPoint) float64 {
	peak := 0.0
	for _, p := range points {
		if !math.IsNaN(p.Value) && !math.IsInf(p.Value, 0) {
			peak = math.Max(peak, p.Value)
		}
	}
	return peak
}

var (
	trendSeries = usage(week5m, 1, func(i int, r *rand.Rand) float64 {
		return 0.5 + float64(i)/week5m + 0.02*r.NormFloat64()
	})
	dailySeries = usage(week5m, 2, func(i int, r *rand.Rand) float64 {
		return 1 + 0.5*dailyWave(i) + 0.05*r.NormFloat64()
	})
	weeklySeries = usage(4*week5m, 3, func(i int, r *rand.Rand) float64 {
		base := 1.0
		if (i/samplesPerDay)%7 >= 5 {
			base = 0.4
		}
		return base + 0.3*dailyWave(i) + 0.03*r.NormFloat64()
	})
	trendDailySeries = usage(week5m, 4, func(i int, r *rand.Rand) float64 {
		return 1 + float64(i)/week5m + 0.4*dailyWave(i) + 0.05*r.NormFloat64()
	})
	noiseSeries = usage(week5m, 5, func(_ int, r *rand.Rand) float64 {
		return math.Abs(1 + 0.1*r.NormFloat64())
	})
	spikeSeries = usage(week5m, 6, func(i int, r *rand.Rand) float64 {
		if i%400 == 7 {
			return 50
		}
		return math.Abs(1 + 0.05*r.NormFloat64())
	})
)

func historyMetrics(cpu, mem []prometheus.TimeSeriesPoint) prometheus.ContainerMetrics {
	m := sampleMetrics()
	m.CPUHistory = cpu
	m.MemoryHistory = mem
	return m
}

func calculateARIMA(t *testing.T, m prometheus.ContainerMetrics, current autoscalingv1alpha1.ResourceRequirements, config StrategyConfig) Recommendation {
	t.Helper()
	s := NewARIMAStrategy(5 * time.Minute)
	recs, err := s.Calculate(context.Background(), []prometheus.ContainerMetrics{m},
		map[string]autoscalingv1alpha1.ResourceRequirements{m.ContainerName: current}, config)
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	if len(recs) != 1 || recs[0].ContainerName != m.ContainerName || recs[0].Method != StrategyNameARIMA {
		t.Fatalf("unexpected recommendations: %+v", recs)
	}
	assertValidRecommendation(t, recs[0])
	return recs[0]
}

// assertValidRecommendation checks that every value is a non-negative,
// canonical Kubernetes quantity and that limits are not below requests.
func assertValidRecommendation(t *testing.T, rec Recommendation) {
	t.Helper()
	check := func(what string, q *resource.Quantity, format resource.Format) {
		if q == nil {
			return
		}
		if q.Sign() < 0 {
			t.Errorf("%s = %s is negative", what, q)
		}
		if q.Format != format {
			t.Errorf("%s format = %s, want %s", what, q.Format, format)
		}
		parsed, err := resource.ParseQuantity(q.String())
		if err != nil || parsed.Cmp(*q) != 0 {
			t.Errorf("%s = %s does not round-trip: %v", what, q, err)
		}
		if format == resource.BinarySI && q.MilliValue()%1000 != 0 {
			t.Errorf("%s = %s is not a whole number of bytes", what, q)
		}
	}
	check("requests.cpu", rec.Requests.CPU, resource.DecimalSI)
	check("limits.cpu", rec.Limits.CPU, resource.DecimalSI)
	check("requests.memory", rec.Requests.Memory, resource.BinarySI)
	check("limits.memory", rec.Limits.Memory, resource.BinarySI)
	if rec.Requests.CPU != nil && rec.Limits.CPU != nil && rec.Limits.CPU.Cmp(*rec.Requests.CPU) < 0 {
		t.Errorf("limits.cpu %s < requests.cpu %s", rec.Limits.CPU, rec.Requests.CPU)
	}
	if rec.Requests.Memory != nil && rec.Limits.Memory != nil && rec.Limits.Memory.Cmp(*rec.Requests.Memory) < 0 {
		t.Errorf("limits.memory %s < requests.memory %s", rec.Limits.Memory, rec.Requests.Memory)
	}
	if math.IsNaN(rec.Confidence) || rec.Confidence <= 0 || rec.Confidence > 1 {
		t.Errorf("confidence = %v", rec.Confidence)
	}
}

func defaultARIMAParams(t *testing.T) arimaParams {
	t.Helper()
	p, err := parseARIMAParameters(nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestARIMAStrategy_Forecasts(t *testing.T) {
	tests := []struct {
		name    string
		history []prometheus.TimeSeriesPoint
		check   func(t *testing.T, f arimaForecast, peak float64)
	}{
		{
			name:    "linear trend is extrapolated above the observed peak",
			history: trendSeries,
			check: func(t *testing.T, f arimaForecast, peak float64) {
				if !strings.HasPrefix(f.model, "ARIMA(") || f.requests <= peak || f.requests > 2*peak {
					t.Errorf("model %s requests %v, peak %v", f.model, f.requests, peak)
				}
			},
		},
		{
			name:    "daily seasonality is modelled",
			history: dailySeries,
			check: func(t *testing.T, f arimaForecast, peak float64) {
				if !strings.HasPrefix(f.model, "SARIMA(") || !strings.HasSuffix(f.model, "[96]") || f.interval != 15*time.Minute || f.samples != 672 {
					t.Errorf("model %s interval %s samples %d", f.model, f.interval, f.samples)
				}
				if f.requests < 0.95*peak || f.requests > 1.3*peak {
					t.Errorf("requests %v, peak %v", f.requests, peak)
				}
			},
		},
		{
			name:    "weekly seasonality is modelled from four weeks",
			history: weeklySeries,
			check: func(t *testing.T, f arimaForecast, peak float64) {
				if !strings.HasPrefix(f.model, "SARIMA(") || !strings.HasSuffix(f.model, "[224]") || f.interval != 45*time.Minute {
					t.Errorf("model %s interval %s", f.model, f.interval)
				}
				if f.requests < 0.95*peak || f.requests > 1.3*peak {
					t.Errorf("requests %v, peak %v", f.requests, peak)
				}
			},
		},
		{
			name:    "trend with daily seasonality",
			history: trendDailySeries,
			check: func(t *testing.T, f arimaForecast, peak float64) {
				if !strings.HasPrefix(f.model, "SARIMA(") || f.requests <= peak || f.requests > 2*peak {
					t.Errorf("model %s requests %v, peak %v", f.model, f.requests, peak)
				}
			},
		},
		{
			name:    "white noise stays around the mean",
			history: noiseSeries,
			check: func(t *testing.T, f arimaForecast, peak float64) {
				if f.requests < 1.1 || f.requests > peak {
					t.Errorf("model %s requests %v, peak %v", f.model, f.requests, peak)
				}
			},
		},
		{
			name:    "huge spikes widen the interval but stay below the peak",
			history: spikeSeries,
			check: func(t *testing.T, f arimaForecast, peak float64) {
				if f.requests < 1.1 || f.requests > peak || f.limits > peak {
					t.Errorf("model %s requests %v limits %v, peak %v", f.model, f.requests, f.limits, peak)
				}
			},
		},
	}

	s := NewARIMAStrategy(5 * time.Minute)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := s.forecast(context.Background(), tt.history, defaultARIMAParams(t))
			if err != nil {
				t.Fatalf("forecast: %v", err)
			}
			if f.limits < f.requests {
				t.Errorf("limits bound %v < requests bound %v", f.limits, f.requests)
			}
			tt.check(t, f, peakOf(tt.history))

			rec := calculateARIMA(t, historyMetrics(tt.history, scaled(tt.history, 512*mib)), autoscalingv1alpha1.ResourceRequirements{}, StrategyConfig{})
			for _, key := range []string{"cpu_fallback", "memory_fallback", "cpu_p95", "memory_p99"} {
				if _, ok := rec.Metadata[key]; ok {
					t.Errorf("unexpected metadata %s = %v", key, rec.Metadata[key])
				}
			}
			if rec.Metadata["cpu_model"] != f.model || rec.Confidence != defaultARIMAConfidence {
				t.Errorf("metadata %v confidence %v", rec.Metadata, rec.Confidence)
			}
			assertQty(t, "requests.cpu", rec.Requests.CPU, applyBuffer(cpuQuantity(f.requests), 20).String())
			assertQty(t, "limits.cpu", rec.Limits.CPU, applyBuffer(cpuQuantity(f.limits), 30).String())
		})
	}
}

func TestARIMAStrategy_Fallback(t *testing.T) {
	gappy := append(append([]prometheus.TimeSeriesPoint{}, dailySeries[:900]...), dailySeries[1500:]...)
	nan := usage(week5m, 7, func(int, *rand.Rand) float64 { return math.NaN() })
	negative := usage(week5m, 8, func(int, *rand.Rand) float64 { return -1 })
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name    string
		ctx     context.Context
		history []prometheus.TimeSeriesPoint
		params  map[string]interface{}
		reason  string
	}{
		{name: "no history", history: nil, reason: "no usage history"},
		{name: "very short series", history: dailySeries[:10], reason: "history too short: 10 valid samples"},
		{name: "constant series", history: usage(week5m, 0, func(int, *rand.Rand) float64 { return 0.25 }), reason: "constant"},
		{name: "all-zero series", history: usage(week5m, 0, func(int, *rand.Rand) float64 { return 0 }), reason: "constant"},
		{name: "history shorter than the horizon", history: dailySeries[:100], reason: "less than the 24h0m0s horizon"},
		{name: "too many gaps", history: gappy, reason: "too many gaps"},
		{name: "only NaN", history: nan, reason: "history too short: 0 valid samples"},
		{name: "only negative values", history: negative, reason: "history too short: 0 valid samples"},
		{
			name:    "fixed seasonal model does not fit the history",
			history: dailySeries,
			params:  map[string]interface{}{"mode": "fixed", "order": []interface{}{1.0, 0.0, 0.0}, "seasonalOrder": []interface{}{1.0, 1.0, 1.0}, "seasonalPeriod": "72h"},
			reason:  "model fit failed: insufficient data points",
		},
		{
			name:    "fixed seasonal period below two samples",
			history: dailySeries,
			params:  map[string]interface{}{"mode": "fixed", "order": []interface{}{1.0, 0.0, 0.0}, "seasonalOrder": []interface{}{1.0, 0.0, 0.0}, "seasonalPeriod": "5m"},
			reason:  "shorter than two 15m0s samples",
		},
		{name: "cancelled analysis", ctx: cancelled, history: dailySeries, reason: "analysis cancelled"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := tt.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			m := historyMetrics(tt.history, tt.history)
			config := StrategyConfig{Parameters: tt.params}
			recs, err := NewARIMAStrategy(5*time.Minute).Calculate(ctx, []prometheus.ContainerMetrics{m}, nil, config)
			if err != nil {
				t.Fatalf("Calculate: %v", err)
			}
			rec := recs[0]
			assertValidRecommendation(t, rec)
			for _, res := range []string{"cpu", "memory"} {
				reason, _ := rec.Metadata[res+"_fallback"].(string)
				if !strings.HasPrefix(reason, "percentile: ") || !strings.Contains(reason, tt.reason) {
					t.Errorf("%s_fallback = %q, want reason containing %q", res, reason, tt.reason)
				}
			}

			want, _ := NewPercentileStrategy().Calculate(ctx, []prometheus.ContainerMetrics{m}, nil, config)
			assertQty(t, "requests.cpu", rec.Requests.CPU, want[0].Requests.CPU.String())
			assertQty(t, "limits.cpu", rec.Limits.CPU, want[0].Limits.CPU.String())
			assertQty(t, "requests.memory", rec.Requests.Memory, want[0].Requests.Memory.String())
			assertQty(t, "limits.memory", rec.Limits.Memory, want[0].Limits.Memory.String())
			if rec.Metadata["cpu_p95"] != want[0].Metadata["cpu_p95"] || rec.Confidence != want[0].Confidence {
				t.Errorf("metadata %v confidence %v", rec.Metadata, rec.Confidence)
			}
		})
	}
}

func TestARIMAStrategy_FallbackPerResource(t *testing.T) {
	rec := calculateARIMA(t, historyMetrics(dailySeries, dailySeries[:10]), autoscalingv1alpha1.ResourceRequirements{}, StrategyConfig{})
	if _, ok := rec.Metadata["cpu_model"]; !ok {
		t.Errorf("cpu should be forecast: %v", rec.Metadata)
	}
	if _, ok := rec.Metadata["memory_fallback"]; !ok {
		t.Errorf("memory should fall back: %v", rec.Metadata)
	}
	assertQty(t, "requests.memory", rec.Requests.Memory, "120Mi")
	assertQty(t, "limits.memory", rec.Limits.Memory, "260Mi")
	if rec.Confidence != 0.8 {
		t.Errorf("confidence = %v, want the percentile confidence 0.8", rec.Confidence)
	}
}

func TestARIMAStrategy_UpperBound(t *testing.T) {
	tests := []struct {
		name   string
		upper  []float64
		want   float64
		reason string
	}{
		{name: "largest value", upper: []float64{1, 3, 2}, want: 3},
		{name: "negative steps are ignored", upper: []float64{-1, 0.5}, want: 0.5},
		{name: "empty", upper: nil, reason: "no forecast"},
		{name: "NaN", upper: []float64{1, math.NaN()}, reason: "non-finite"},
		{name: "infinite", upper: []float64{math.Inf(1)}, reason: "non-finite"},
		{name: "non-positive", upper: []float64{-2, 0}, reason: "no usage"},
		{name: "diverging", upper: []float64{1, 11}, reason: "diverges"},
		{name: "beyond the quantity range", upper: []float64{2e15}, reason: "diverges"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			peak := 1.0
			if tt.name == "beyond the quantity range" {
				peak = 1e15
			}
			got, err := upperBound(tt.upper, peak)
			if tt.reason != "" {
				if err == nil || !strings.Contains(err.Error(), tt.reason) {
					t.Errorf("err = %v, want %q", err, tt.reason)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("upperBound = %v, %v, want %v", got, err, tt.want)
			}
		})
	}
}

func TestARIMAStrategy_GapsAndInvalidSamples(t *testing.T) {
	r := rand.New(rand.NewSource(9))
	var history []prometheus.TimeSeriesPoint
	for i, p := range dailySeries {
		switch {
		case i%20 == 3:
			continue // missing scrape
		case i%10 == 5:
			p.Value = math.NaN()
		case i%97 == 0:
			p.Value = math.Inf(1)
		case i%101 == 0:
			p.Value = -5
		}
		history = append(history, p)
		if i%50 == 0 {
			history = append(history, p) // duplicate sample
		}
	}
	r.Shuffle(len(history), func(i, j int) { history[i], history[j] = history[j], history[i] })

	f, err := NewARIMAStrategy(5*time.Minute).forecast(context.Background(), history, defaultARIMAParams(t))
	if err != nil {
		t.Fatalf("forecast: %v", err)
	}
	if !strings.HasPrefix(f.model, "SARIMA(") || f.requests < peakOf(dailySeries)*0.9 || f.requests > peakOf(dailySeries)*1.5 {
		t.Errorf("model %s requests %v", f.model, f.requests)
	}
}

func TestARIMAStrategy_PrepareSeries(t *testing.T) {
	s := NewARIMAStrategy(5 * time.Minute)

	values, start, interval, err := s.prepareSeries(usage(30*samplesPerDay, 1, func(i int, _ *rand.Rand) float64 { return float64(i) }), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) > 1000 || interval != 45*time.Minute || start != testStart || int64(arimaDay)%int64(interval) != 0 {
		t.Errorf("len %d interval %s start %d", len(values), interval, start)
	}
	if values[0] != 8 || values[1] != 17 {
		t.Errorf("buckets keep the max sample: %v", values[:2])
	}

	gaps := usage(60, 1, func(i int, _ *rand.Rand) float64 { return float64(i) })
	gaps = append(gaps[:10], gaps[13:]...)
	values, _, interval, err = s.prepareSeries(gaps, 1000)
	if err != nil || interval != 5*time.Minute || len(values) != 60 {
		t.Fatalf("len %d interval %s err %v", len(values), interval, err)
	}
	for i := 9; i <= 13; i++ {
		if values[i] != float64(i) {
			t.Errorf("values[%d] = %v, want linear interpolation %d", i, values[i], i)
		}
	}

	hourly := make([]prometheus.TimeSeriesPoint, 200)
	for i := range hourly {
		hourly[i] = prometheus.TimeSeriesPoint{Timestamp: testStart + int64(i)*3600, Value: float64(i % 24)}
	}
	if _, _, interval, err = s.prepareSeries(hourly, 1000); err != nil || interval != time.Hour {
		t.Errorf("coarser history keeps its resolution: %s %v", interval, err)
	}

	same := make([]prometheus.TimeSeriesPoint, 60)
	if _, _, _, err = s.prepareSeries(same, 1000); err == nil || !strings.Contains(err.Error(), "one timestamp") {
		t.Errorf("err = %v", err)
	}

	stale := append([]prometheus.TimeSeriesPoint{{Timestamp: 0, Value: 1}}, usage(100, 1, func(int, *rand.Rand) float64 { return 1 })...)
	if _, _, _, err = s.prepareSeries(stale, 1000); err == nil || !strings.Contains(err.Error(), "gaps") {
		t.Errorf("a single stale sample leaves the buckets empty: %v", err)
	}

	extreme := usage(100, 1, func(int, *rand.Rand) float64 { return 1 })
	extreme[0].Timestamp, extreme[99].Timestamp = math.MinInt64, math.MaxInt64
	if _, _, _, err = s.prepareSeries(extreme, 1000); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Errorf("err = %v", err)
	}
}

func TestARIMAStrategy_SeasonalPeriods(t *testing.T) {
	tests := []struct {
		interval time.Duration
		n        int
		want     []int
	}{
		{15 * time.Minute, 672, []int{96}},
		{45 * time.Minute, 896, []int{32, 224}},
		{7 * time.Minute, 1000, []int{206}},
		{6 * time.Hour, 1000, []int{4, 28}},
		{12 * time.Hour, 1000, []int{14}},
		{2 * 24 * time.Hour, 1000, nil}, // a week is 3.5 samples
		{15 * time.Minute, 200, nil},
	}
	for _, tt := range tests {
		if got := seasonalPeriods(tt.interval, tt.n); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("seasonalPeriods(%s, %d) = %v, want %v", tt.interval, tt.n, got, tt.want)
		}
	}
}

func TestARIMAStrategy_Modes(t *testing.T) {
	tests := []struct {
		name   string
		params map[string]interface{}
		model  string
	}{
		{name: "non-seasonal automatic selection", params: map[string]interface{}{"seasonal": false}, model: "ARIMA("},
		{name: "fixed ARIMA", params: map[string]interface{}{"mode": "fixed", "order": []interface{}{1.0, 1.0, 1.0}}, model: "ARIMA(1,1,1)"},
		{
			name:   "fixed SARIMA with a daily period",
			params: map[string]interface{}{"mode": "fixed", "order": []interface{}{1.0, 0.0, 0.0}, "seasonalOrder": []interface{}{1.0, 1.0, 0.0}, "seasonalPeriod": "24h"},
			model:  "SARIMA(1,0,0)(1,1,0)[96]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := calculateARIMA(t, historyMetrics(dailySeries, scaled(dailySeries, mib)), autoscalingv1alpha1.ResourceRequirements{}, StrategyConfig{Parameters: tt.params})
			for _, key := range []string{"cpu_model", "memory_model"} {
				if model, _ := rec.Metadata[key].(string); !strings.HasPrefix(model, tt.model) {
					t.Errorf("%s = %q, want %q (%v)", key, model, tt.model, rec.Metadata)
				}
			}
		})
	}
}

func TestARIMAStrategy_PostProcessing(t *testing.T) {
	m := historyMetrics(dailySeries, scaled(dailySeries, 512*mib))
	f, err := NewARIMAStrategy(5*time.Minute).forecast(context.Background(), dailySeries, defaultARIMAParams(t))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("buffers from parameters", func(t *testing.T) {
		rec := calculateARIMA(t, m, autoscalingv1alpha1.ResourceRequirements{}, StrategyConfig{Parameters: map[string]interface{}{
			"requestsBufferPercent": 0.0, "limitsBufferPercent": 10.0, "horizon": "24h", "confidence": 0.95,
		}})
		assertQty(t, "requests.cpu", rec.Requests.CPU, cpuQuantity(f.requests).String())
		assertQty(t, "limits.cpu", rec.Limits.CPU, applyBuffer(cpuQuantity(f.limits), 10).String())
		if rec.Metadata["requests_buffer_percent"] != 0.0 || rec.Metadata["cpu_upper_bound"] != cpuQuantity(f.requests).String() {
			t.Errorf("metadata %v", rec.Metadata)
		}
	})

	t.Run("gradual change from current resources", func(t *testing.T) {
		rec := calculateARIMA(t, m, autoscalingv1alpha1.ResourceRequirements{
			Requests: &autoscalingv1alpha1.ResourceValues{CPU: qty("100m"), Memory: qty("100Mi")},
		}, StrategyConfig{})
		assertQty(t, "requests.cpu", rec.Requests.CPU, "150m")
		assertQty(t, "requests.memory", rec.Requests.Memory, "150Mi")
	})

	t.Run("safety rails", func(t *testing.T) {
		rec := calculateARIMA(t, m, autoscalingv1alpha1.ResourceRequirements{}, StrategyConfig{SafetyRails: SafetyRailsConfig{
			MinCPU: qty("4"), MaxCPU: qty("4"), MinMemory: qty("1Gi"), MaxMemory: qty("1Gi"),
		}})
		assertQty(t, "requests.cpu", rec.Requests.CPU, "4")
		assertQty(t, "limits.cpu", rec.Limits.CPU, "4")
		assertQty(t, "requests.memory", rec.Requests.Memory, "1Gi")
		assertQty(t, "limits.memory", rec.Limits.Memory, "1Gi")
		if !rec.Capped {
			t.Error("expected capped recommendation")
		}
	})

	t.Run("quota", func(t *testing.T) {
		rec := calculateARIMA(t, m, autoscalingv1alpha1.ResourceRequirements{}, StrategyConfig{ResourceQuota: &ResourceQuotaConfig{
			CPULimit: qty("1m"), MemoryLimit: qty("1Ki"),
		}})
		assertQty(t, "limits.cpu", rec.Limits.CPU, "1m")
		assertQty(t, "limits.memory", rec.Limits.Memory, "1Ki")
		if rec.CappedReason != cappedAtQuotaMemory {
			t.Errorf("capped reason = %q", rec.CappedReason)
		}
	})

	t.Run("memory is a whole number of bytes", func(t *testing.T) {
		rec := calculateARIMA(t, m, autoscalingv1alpha1.ResourceRequirements{}, StrategyConfig{})
		if rec.Requests.Memory.Value() < 512*mib {
			t.Errorf("requests.memory = %s", rec.Requests.Memory)
		}
		if got := memoryQuantity(1.2); got.Value() != 2 || got.String() != "2" {
			t.Errorf("memoryQuantity rounds up to whole bytes: %s", got)
		}
		if got := cpuQuantity(0.0001); got.String() != "1m" {
			t.Errorf("cpuQuantity rounds up to whole millicores: %s", got)
		}
	})
}

func TestARIMAStrategy_Deterministic(t *testing.T) {
	m := historyMetrics(trendDailySeries, scaled(weeklySeries, 256*mib))
	first := calculateARIMA(t, m, autoscalingv1alpha1.ResourceRequirements{}, StrategyConfig{})
	for i := 0; i < 3; i++ {
		if again := calculateARIMA(t, m, autoscalingv1alpha1.ResourceRequirements{}, StrategyConfig{}); !reflect.DeepEqual(first, again) {
			t.Fatalf("run %d differs:\n%+v\n%+v", i, first, again)
		}
	}
}

// TestARIMAStrategy_PathologicalInputs feeds random degenerate histories and
// parameters to the strategy: it must never panic and always return valid,
// non-negative quantities.
func TestARIMAStrategy_PathologicalInputs(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	steps := []int64{0, 1, 15, 300, 3600, 86400}
	magnitudes := []float64{1e-300, 1e-9, 1e-3, 1, 1e6, 1e12, 1e300}
	paramSets := []map[string]interface{}{
		nil,
		{"seasonal": false, "maxSamples": 100.0},
		{"horizon": "5m", "confidence": 0.5, "limitsConfidence": 0.999},
		{"horizon": "168h", "maxSamples": 5000.0},
		{"mode": "fixed", "order": []interface{}{5.0, 2.0, 5.0}},
		{"mode": "fixed", "order": []interface{}{0.0, 2.0, 0.0}, "horizon": "168h"},
		{"mode": "fixed", "order": []interface{}{2.0, 1.0, 2.0}, "seasonalOrder": []interface{}{2.0, 1.0, 2.0}, "seasonalPeriod": "1h"},
	}
	slowest := time.Duration(0)
	for iter := 0; iter < 150; iter++ {
		n := r.Intn(3000)
		step := steps[r.Intn(len(steps))]
		scale := magnitudes[r.Intn(len(magnitudes))]
		history := make([]prometheus.TimeSeriesPoint, n)
		for i := range history {
			v := scale * (1 + 0.5*math.Sin(float64(i)/10) + 0.2*r.NormFloat64())
			switch x := r.Float64(); {
			case x < 0.05:
				v = math.NaN()
			case x < 0.07:
				v = math.Inf(1 - 2*r.Intn(2))
			case x < 0.10:
				v = -v
			case x < 0.12:
				v *= 1e6
			case x < 0.20:
				v = 0
			}
			history[i] = prometheus.TimeSeriesPoint{Timestamp: testStart + int64(i)*step + r.Int63n(step+1), Value: v}
		}
		if r.Intn(2) == 0 {
			r.Shuffle(n, func(i, j int) { history[i], history[j] = history[j], history[i] })
		}
		m := historyMetrics(history, history)
		params := paramSets[r.Intn(len(paramSets))]
		start := time.Now()
		recs, err := NewARIMAStrategy(5*time.Minute).Calculate(context.Background(), []prometheus.ContainerMetrics{m}, nil, StrategyConfig{Parameters: params})
		slowest = max(slowest, time.Since(start))
		if err != nil {
			t.Fatalf("iteration %d: %v", iter, err)
		}
		assertValidRecommendation(t, recs[0])
	}
	t.Logf("slowest calculation: %s", slowest)
}

func TestARIMAStrategy_ValidateFitProfileParameters(t *testing.T) {
	order := func(v ...interface{}) []interface{} { return v }
	tests := []struct {
		name    string
		params  map[string]interface{}
		wantErr string
	}{
		{name: "nil"},
		{name: "empty", params: map[string]interface{}{}},
		{name: "all auto parameters", params: map[string]interface{}{
			"mode": "auto", "seasonal": true, "confidence": 0.9, "limitsConfidence": 0.99, "horizon": "12h", "maxSamples": 500.0,
			"requestsBufferPercent": 10.0, "limitsBufferPercent": 20.0, "maxChangePercent": 100.0,
		}},
		{name: "fixed ARIMA", params: map[string]interface{}{"mode": "fixed", "order": order(2.0, 1.0, 2.0)}},
		{name: "fixed SARIMA", params: map[string]interface{}{"mode": "fixed", "order": order(1.0, 0.0, 1.0), "seasonalOrder": order(1.0, 1.0, 1.0), "seasonalPeriod": "168h"}},
		{name: "fixed with zero seasonal order", params: map[string]interface{}{"mode": "fixed", "order": []int{1, 1, 0}, "seasonalOrder": []float64{0, 0, 0}}},

		{name: "unknown key", params: map[string]interface{}{"windowSize": 3.0}, wantErr: `unknown parameter "windowSize"`},
		{name: "DSP-only key", params: map[string]interface{}{"enablePeakDetection": true}, wantErr: "unknown parameter"},
		{name: "unknown mode", params: map[string]interface{}{"mode": "magic"}, wantErr: "mode must be"},
		{name: "mode not a string", params: map[string]interface{}{"mode": 1.0}, wantErr: "mode must be"},

		{name: "confidence zero", params: map[string]interface{}{"confidence": 0.0}, wantErr: "confidence must be between 0.5 and 0.999"},
		{name: "confidence one", params: map[string]interface{}{"confidence": 1.0}, wantErr: "confidence must be between"},
		{name: "confidence above one", params: map[string]interface{}{"confidence": 95.0}, wantErr: "confidence must be between"},
		{name: "confidence NaN", params: map[string]interface{}{"confidence": math.NaN()}, wantErr: "confidence must be between"},
		{name: "confidence as string", params: map[string]interface{}{"confidence": "0.95"}, wantErr: "confidence must be a number"},
		{name: "limits confidence below confidence", params: map[string]interface{}{"confidence": 0.99, "limitsConfidence": 0.9}, wantErr: "limitsConfidence (0.9) must be >= confidence (0.99)"},
		{name: "limits confidence out of range", params: map[string]interface{}{"limitsConfidence": 1.2}, wantErr: "limitsConfidence must be between"},

		{name: "horizon not a string", params: map[string]interface{}{"horizon": 24.0}, wantErr: "horizon must be a duration string"},
		{name: "horizon unparsable", params: map[string]interface{}{"horizon": "a day"}, wantErr: "horizon:"},
		{name: "horizon negative", params: map[string]interface{}{"horizon": "-1h"}, wantErr: "horizon must be positive"},
		{name: "horizon too long", params: map[string]interface{}{"horizon": "200h"}, wantErr: "at most 168h0m0s"},
		{name: "max samples too small", params: map[string]interface{}{"maxSamples": 50.0}, wantErr: "maxSamples must be between 100 and 5000"},
		{name: "max samples too large", params: map[string]interface{}{"maxSamples": 50000.0}, wantErr: "maxSamples must be between"},
		{name: "max samples fractional", params: map[string]interface{}{"maxSamples": 500.5}, wantErr: "whole number"},
		{name: "buffer out of range", params: map[string]interface{}{"requestsBufferPercent": -1.0}, wantErr: "requestsBufferPercent must be between 0 and 1000"},
		{name: "buffer as string", params: map[string]interface{}{"limitsBufferPercent": "20"}, wantErr: "limitsBufferPercent must be a number"},
		{name: "max change out of range", params: map[string]interface{}{"maxChangePercent": 2000.0}, wantErr: "maxChangePercent must be between"},
		{name: "seasonal not a bool", params: map[string]interface{}{"seasonal": "yes"}, wantErr: "seasonal must be a boolean"},

		{name: "order in auto mode", params: map[string]interface{}{"order": order(1.0, 1.0, 1.0)}, wantErr: "order requires mode 'fixed'"},
		{name: "seasonal period in auto mode", params: map[string]interface{}{"seasonalPeriod": "24h"}, wantErr: "seasonalPeriod requires mode 'fixed'"},
		{name: "seasonal in fixed mode", params: map[string]interface{}{"mode": "fixed", "order": order(1.0, 0.0, 0.0), "seasonal": true}, wantErr: "seasonal requires mode 'auto'"},
		{name: "fixed without order", params: map[string]interface{}{"mode": "fixed"}, wantErr: "requires order"},
		{name: "order negative", params: map[string]interface{}{"mode": "fixed", "order": order(-1.0, 0.0, 0.0)}, wantErr: "order[0] must be an integer between 0 and 5"},
		{name: "order too large", params: map[string]interface{}{"mode": "fixed", "order": order(1.0, 3.0, 0.0)}, wantErr: "order[1] must be an integer between 0 and 2"},
		{name: "order fractional", params: map[string]interface{}{"mode": "fixed", "order": order(1.0, 0.0, 0.5)}, wantErr: "order[2]"},
		{name: "order wrong length", params: map[string]interface{}{"mode": "fixed", "order": order(1.0, 0.0)}, wantErr: "order must have 3 elements"},
		{name: "order not an array", params: map[string]interface{}{"mode": "fixed", "order": "1,1,1"}, wantErr: "order must be an array"},
		{name: "order element not a number", params: map[string]interface{}{"mode": "fixed", "order": order("1", 0.0, 0.0)}, wantErr: "order[0]"},
		{name: "seasonal order too large", params: map[string]interface{}{"mode": "fixed", "order": order(1.0, 0.0, 0.0), "seasonalOrder": order(0.0, 2.0, 0.0), "seasonalPeriod": "24h"}, wantErr: "seasonalOrder[1] must be an integer between 0 and 1"},
		{name: "seasonal order without period", params: map[string]interface{}{"mode": "fixed", "order": order(1.0, 0.0, 0.0), "seasonalOrder": order(1.0, 0.0, 0.0)}, wantErr: "seasonalOrder requires seasonalPeriod"},
		{name: "period without seasonal order", params: map[string]interface{}{"mode": "fixed", "order": order(1.0, 0.0, 0.0), "seasonalPeriod": "24h"}, wantErr: "seasonalPeriod requires a non-zero seasonalOrder"},
		{name: "seasonal period too long", params: map[string]interface{}{"mode": "fixed", "order": order(1.0, 0.0, 0.0), "seasonalOrder": order(1.0, 0.0, 0.0), "seasonalPeriod": "720h"}, wantErr: "seasonalPeriod must be positive and at most"},
	}

	s := NewARIMAStrategy(5 * time.Minute)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := s.ValidateFitProfileParameters(tt.params)
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want %q", err, tt.wantErr)
			}
			if err := s.ValidateConfig(StrategyConfig{Parameters: tt.params}); err == nil {
				t.Error("ValidateConfig accepted invalid parameters")
			}
			if _, err := s.Calculate(context.Background(), []prometheus.ContainerMetrics{sampleMetrics()}, nil, StrategyConfig{Parameters: tt.params}); err == nil {
				t.Error("Calculate accepted invalid parameters")
			}
		})
	}
}

func TestARIMAStrategy_JSONParameters(t *testing.T) {
	var params map[string]interface{}
	if err := json.Unmarshal([]byte(`{"mode":"fixed","order":[1,1,1],"seasonalOrder":[1,1,0],"seasonalPeriod":"24h","horizon":"6h","maxSamples":800}`), &params); err != nil {
		t.Fatal(err)
	}
	p, err := parseARIMAParameters(params)
	if err != nil {
		t.Fatal(err)
	}
	if p.order != [3]int{1, 1, 1} || p.seasonalOrder != [3]int{1, 1, 0} || p.seasonalPeriod != 24*time.Hour ||
		p.horizon != 6*time.Hour || p.maxSamples != 800 || p.confidence != 0.95 || p.limitsConfidence != 0.99 {
		t.Errorf("parsed %+v", p)
	}
}

func TestARIMAStrategy_SampleFitProfile(t *testing.T) {
	data, err := os.ReadFile("../../../config/samples/fitprofile-arima.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var fp autoscalingv1alpha1.FitProfile
	if err := yaml.UnmarshalStrict(data, &fp); err != nil {
		t.Fatal(err)
	}
	if fp.Spec.Strategy != StrategyNameARIMA || fp.Spec.Parameters == nil {
		t.Fatalf("unexpected sample spec: %+v", fp.Spec)
	}
	var params map[string]interface{}
	if err := json.Unmarshal(fp.Spec.Parameters.Raw, &params); err != nil {
		t.Fatal(err)
	}
	if err := NewARIMAStrategy(0).ValidateFitProfileParameters(params); err != nil {
		t.Errorf("sample parameters are invalid: %v", err)
	}
	example, err := json.Marshal(NewARIMAStrategy(0).GetDefaultFitProfileSpec().ExampleParameters)
	if err != nil {
		t.Fatal(err)
	}
	var exampleParams map[string]interface{}
	if err := json.Unmarshal(example, &exampleParams); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(params, exampleParams) {
		t.Errorf("sample parameters %v differ from the example parameters %v", params, exampleParams)
	}
}

func TestARIMAStrategy_DefaultSpecAndHistoryStep(t *testing.T) {
	s := NewARIMAStrategy(0)
	if s.Name() != StrategyNameARIMA || s.sampleInterval != DefaultARIMASampleInterval {
		t.Errorf("name %s interval %s", s.Name(), s.sampleInterval)
	}
	var _ HistoryStrategy = s

	spec := s.GetDefaultFitProfileSpec()
	if spec.Strategy != StrategyNameARIMA || spec.ParametersSchema["type"] != "object" {
		t.Errorf("unexpected spec: %+v", spec)
	}
	properties, _ := spec.ParametersSchema["properties"].(map[string]interface{})
	if len(properties) != len(arimaParameterKeys) {
		t.Errorf("schema documents %d parameters, %d are accepted", len(properties), len(arimaParameterKeys))
	}
	for key := range arimaParameterKeys {
		if _, ok := properties[key]; !ok {
			t.Errorf("parameter %s missing from the schema", key)
		}
	}
	if err := s.ValidateFitProfileParameters(spec.ExampleParameters); err != nil {
		t.Errorf("example parameters must be valid: %v", err)
	}
	raw, _ := json.Marshal(spec.ExampleParameters)
	var fromJSON map[string]interface{}
	_ = json.Unmarshal(raw, &fromJSON)
	if err := s.ValidateFitProfileParameters(fromJSON); err != nil {
		t.Errorf("example parameters must be valid after a JSON round trip: %v", err)
	}

	for _, tt := range []struct {
		window time.Duration
		want   time.Duration
	}{
		{7 * 24 * time.Hour, 5 * time.Minute},
		{28 * 24 * time.Hour, 5 * time.Minute},
		{90 * 24 * time.Hour, 778 * time.Second},
	} {
		got := s.HistoryStep(tt.window)
		if got != tt.want || tt.window/got > prometheus.MaxHistoryPoints {
			t.Errorf("HistoryStep(%s) = %s, want %s", tt.window, got, tt.want)
		}
	}
}

func TestARIMAStrategy_GetChartLines(t *testing.T) {
	s := NewARIMAStrategy(5 * time.Minute)
	metrics := []prometheus.ContainerMetrics{historyMetrics(dailySeries, dailySeries[:10])}
	recs := []Recommendation{{ContainerName: "app"}, {ContainerName: "missing"}}
	lines, err := s.GetChartLines(context.Background(), metrics, nil, recs, StrategyConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || len(lines["cpu_forecast"]) != 96 || len(lines["cpu_forecast_upper"]) != 96 {
		t.Fatalf("lines: %d keys, %d forecast points", len(lines), len(lines["cpu_forecast"]))
	}
	last := dailySeries[len(dailySeries)-1].Timestamp
	for ts, v := range lines["cpu_forecast_upper"] {
		if ts <= last || ts > last+int64(25*time.Hour/time.Second) || v < lines["cpu_forecast"][ts] || v < 0 {
			t.Errorf("point %d = %v (forecast %v)", ts, v, lines["cpu_forecast"][ts])
		}
	}
	if _, err := s.GetChartLines(context.Background(), metrics, nil, recs, StrategyConfig{Parameters: map[string]interface{}{"bad": 1.0}}); err == nil {
		t.Error("expected an error for invalid parameters")
	}
}

func benchmarkForecast(b *testing.B, history []prometheus.TimeSeriesPoint, params map[string]interface{}) {
	s := NewARIMAStrategy(5 * time.Minute)
	p, err := parseARIMAParameters(params)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := s.forecast(context.Background(), history, p); err != nil {
			b.Fatal(err)
		}
	}
}

// Seven days at 5m (2016 samples, downsampled to 672 at 15m).
func BenchmarkARIMAForecast_7d(b *testing.B) { benchmarkForecast(b, trendDailySeries, nil) }

// Four weeks at 5m (8064 samples, downsampled to 896 at 45m) with daily and
// weekly candidates.
func BenchmarkARIMAForecast_28d(b *testing.B) { benchmarkForecast(b, weeklySeries, nil) }

// The largest configurable series: four weeks with maxSamples 5000.
func BenchmarkARIMAForecast_28dMaxSamples(b *testing.B) {
	benchmarkForecast(b, weeklySeries, map[string]interface{}{"maxSamples": 5000.0})
}
