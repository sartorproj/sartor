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
	"errors"
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/sartorproj/sartor/internal/metrics/prometheus"
)

func mustForecastConfig(t *testing.T, horizon time.Duration, confidence float64, model map[string]interface{}) ForecastConfig {
	t.Helper()
	cfg, err := NewForecastConfig(horizon, confidence, model)
	if err != nil {
		t.Fatalf("NewForecastConfig: %v", err)
	}
	return cfg
}

func TestNewForecastConfig(t *testing.T) {
	fixed := map[string]interface{}{"mode": "fixed", "order": []interface{}{1, 0, 0}}
	tests := []struct {
		name       string
		horizon    time.Duration
		confidence float64
		model      map[string]interface{}
		err        string
	}{
		{name: "defaults", horizon: 24 * time.Hour, confidence: 0.95},
		{name: "longest horizon", horizon: 168 * time.Hour, confidence: 0.95},
		{name: "shortest horizon", horizon: time.Nanosecond, confidence: 0.95},
		{name: "low confidence", horizon: time.Hour, confidence: 0.01},
		{name: "high confidence", horizon: time.Hour, confidence: 0.999999},
		{name: "fixed order", horizon: time.Hour, confidence: 0.95, model: fixed},
		{name: "auto without seasonality", horizon: time.Hour, confidence: 0.95, model: map[string]interface{}{"seasonal": false}},
		{name: "zero horizon", horizon: 0, confidence: 0.95, err: "horizon must be positive"},
		{name: "negative horizon", horizon: -time.Hour, confidence: 0.95, err: "horizon must be positive"},
		{name: "horizon too long", horizon: 169 * time.Hour, confidence: 0.95, err: "at most 168h0m0s"},
		{name: "zero confidence", horizon: time.Hour, confidence: 0, err: "confidence must be"},
		{name: "confidence of one", horizon: time.Hour, confidence: 1, err: "confidence must be"},
		{name: "negative confidence", horizon: time.Hour, confidence: -0.5, err: "confidence must be"},
		{name: "NaN confidence", horizon: time.Hour, confidence: math.NaN(), err: "confidence must be"},
		{name: "infinite confidence", horizon: time.Hour, confidence: math.Inf(1), err: "confidence must be"},
		{name: "non-model parameter", horizon: time.Hour, confidence: 0.95, model: map[string]interface{}{"maxSamples": 5000}, err: `unknown model parameter "maxSamples"`},
		{name: "fixed without order", horizon: time.Hour, confidence: 0.95, model: map[string]interface{}{"mode": "fixed"}, err: "requires order"},
		{name: "invalid mode", horizon: time.Hour, confidence: 0.95, model: map[string]interface{}{"mode": "best"}, err: "mode must be"},
		{name: "order out of range", horizon: time.Hour, confidence: 0.95, model: map[string]interface{}{"mode": "fixed", "order": []interface{}{9, 0, 0}}, err: "order[0]"},
		{name: "order is not a number", horizon: time.Hour, confidence: 0.95, model: map[string]interface{}{"mode": "fixed", "order": []interface{}{"a", 0, 0}}, err: "order[0]"},
		{name: "seasonal is not a boolean", horizon: time.Hour, confidence: 0.95, model: map[string]interface{}{"seasonal": "yes"}, err: "seasonal must be a boolean"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewForecastConfig(tt.horizon, tt.confidence, tt.model)
			if tt.err == "" {
				if err != nil {
					t.Errorf("NewForecastConfig: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("err = %v, want %q", err, tt.err)
			}
		})
	}
}

func TestARIMAStrategy_Forecast(t *testing.T) {
	s := NewARIMAStrategy(5 * time.Minute)
	ctx := context.Background()
	fc, err := s.Forecast(ctx, dailySeries, mustForecastConfig(t, 24*time.Hour, 0.95, nil))
	if err != nil {
		t.Fatalf("Forecast: %v", err)
	}

	// The forecast is the one Calculate sizes for.
	f, err := s.forecast(ctx, dailySeries, defaultARIMAParams(t))
	if err != nil {
		t.Fatalf("forecast: %v", err)
	}
	if fc.Model != f.model || fc.SampleInterval != 15*time.Minute || fc.Samples != 672 {
		t.Errorf("model %s interval %s samples %d, want %s 15m 672", fc.Model, fc.SampleInterval, fc.Samples, f.model)
	}
	if len(fc.Points) != 96 {
		t.Fatalf("%d points, want 96 (24h at 15m)", len(fc.Points))
	}
	maxUpper := 0.0
	for i, p := range fc.Points {
		if want := testStart + int64(672+i)*900; p.Timestamp != want {
			t.Errorf("point %d timestamp %d, want %d", i, p.Timestamp, want)
		}
		if p.Lower < 0 || p.Lower > p.Value || p.Value > p.Upper {
			t.Errorf("point %d not ordered: %+v", i, p)
		}
		if math.Abs(p.Value-f.point[i]) > 1e-12 {
			t.Errorf("point %d value %v, want %v", i, p.Value, f.point[i])
		}
		maxUpper = math.Max(maxUpper, p.Upper)
	}
	if math.Abs(maxUpper-f.requests) > 1e-12 {
		t.Errorf("largest upper bound %v, want the requests bound %v", maxUpper, f.requests)
	}

	wide, err := s.Forecast(ctx, dailySeries, mustForecastConfig(t, 24*time.Hour, 0.99, nil))
	if err != nil {
		t.Fatalf("Forecast: %v", err)
	}
	for i := range wide.Points {
		if wide.Points[i].Upper < fc.Points[i].Upper || wide.Points[i].Lower > fc.Points[i].Lower {
			t.Fatalf("point %d: 99%% interval %+v is not wider than 95%% interval %+v", i, wide.Points[i], fc.Points[i])
		}
	}
}

func TestARIMAStrategy_ForecastOptions(t *testing.T) {
	s := NewARIMAStrategy(5 * time.Minute)
	tests := []struct {
		name       string
		history    []prometheus.TimeSeriesPoint
		horizon    time.Duration
		confidence float64
		model      map[string]interface{}
		points     int
		modelName  string
	}{
		{name: "one step for a tiny horizon", history: dailySeries, horizon: time.Nanosecond, confidence: 0.95, points: 1},
		{name: "horizon as long as the history", history: dailySeries, horizon: 168 * time.Hour, confidence: 0.95, points: 672},
		{name: "odd horizon rounds up", history: dailySeries, horizon: 61 * time.Minute, confidence: 0.95, points: 5},
		{name: "very low confidence", history: noiseSeries, horizon: time.Hour, confidence: 0.001, points: 4},
		{name: "very high confidence", history: noiseSeries, horizon: time.Hour, confidence: 0.999999, points: 4},
		{
			name: "fixed order", history: trendSeries, horizon: time.Hour, confidence: 0.95, points: 4,
			model: map[string]interface{}{"mode": "fixed", "order": []interface{}{1, 1, 0}}, modelName: "ARIMA(1,1,0)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc, err := s.Forecast(context.Background(), tt.history, mustForecastConfig(t, tt.horizon, tt.confidence, tt.model))
			if err != nil {
				t.Fatalf("Forecast: %v", err)
			}
			if len(fc.Points) != tt.points {
				t.Errorf("%d points, want %d", len(fc.Points), tt.points)
			}
			if tt.modelName != "" && fc.Model != tt.modelName {
				t.Errorf("model %s, want %s", fc.Model, tt.modelName)
			}
			for i, p := range fc.Points {
				if p.Lower < 0 || p.Lower > p.Value || p.Value > p.Upper || math.IsNaN(p.Upper) {
					t.Errorf("point %d not ordered: %+v", i, p)
				}
			}
		})
	}
}

func TestARIMAStrategy_ForecastErrors(t *testing.T) {
	gappy := append(append([]prometheus.TimeSeriesPoint{}, dailySeries[:900]...), dailySeries[1500:]...)
	mostlyNaN := append([]prometheus.TimeSeriesPoint{}, dailySeries...)
	for i := 700; i < len(mostlyNaN)-1; i++ {
		mostlyNaN[i].Value = math.NaN()
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()

	tests := []struct {
		name    string
		ctx     context.Context
		history []prometheus.TimeSeriesPoint
		err     string
		ctxErr  error
	}{
		{name: "no history", history: nil, err: "no usage history"},
		{name: "very short series", history: dailySeries[:10], err: "history too short: 10 valid samples"},
		{name: "history shorter than the horizon", history: dailySeries[:100], err: "less than the 24h0m0s horizon"},
		{name: "constant series", history: usage(week5m, 0, func(int, *rand.Rand) float64 { return 0.25 }), err: "constant"},
		{name: "all-zero series", history: usage(week5m, 0, func(int, *rand.Rand) float64 { return 0 }), err: "constant"},
		{name: "too many gaps", history: gappy, err: "too many gaps"},
		{name: "mostly NaN", history: mostlyNaN, err: "too many gaps"},
		{name: "only negative values", history: usage(week5m, 8, func(int, *rand.Rand) float64 { return -1 }), err: "0 valid samples"},
		{name: "cancelled", ctx: cancelled, history: dailySeries, err: "cancelled", ctxErr: context.Canceled},
		{name: "deadline exceeded", ctx: expired, history: dailySeries, err: "cancelled", ctxErr: context.DeadlineExceeded},
	}
	s := NewARIMAStrategy(5 * time.Minute)
	cfg := mustForecastConfig(t, 24*time.Hour, 0.95, nil)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := tt.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			fc, err := s.Forecast(ctx, tt.history, cfg)
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Fatalf("err = %v, want %q", err, tt.err)
			}
			if tt.ctxErr != nil && !errors.Is(err, tt.ctxErr) {
				t.Errorf("err = %v does not wrap %v", err, tt.ctxErr)
			}
			if len(fc.Points) != 0 {
				t.Errorf("unexpected points on error: %d", len(fc.Points))
			}
		})
	}
}
