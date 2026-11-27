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
	"fmt"
	"math"
	"slices"
	"sort"
	"time"

	"github.com/sartorproj/sartor/internal/metrics/prometheus"
)

// ARIMAForecastModelKeys are the ARIMA parameters that select the model of a
// forecast; see NewForecastConfig.
var ARIMAForecastModelKeys = []string{paramMode, paramSeasonal, paramOrder, paramSeasonalOrder, paramSeasonalPeriod}

// ForecastPoint is one step of a usage forecast.
type ForecastPoint struct {
	Timestamp int64   `json:"timestamp"`
	Value     float64 `json:"value"`
	Lower     float64 `json:"lower"`
	Upper     float64 `json:"upper"`
}

// Forecast is the forecast of one usage history.
type Forecast struct {
	// Model is the fitted model, e.g. "ARIMA(1,1,1)".
	Model string
	// SampleInterval is the interval of the series the model was fitted to,
	// and of Points.
	SampleInterval time.Duration
	// Samples is the number of samples the model was fitted to.
	Samples int
	// Points cover the horizon, starting one interval after the history.
	Points []ForecastPoint
}

// ForecastConfig is a validated configuration of ARIMAStrategy.Forecast.
type ForecastConfig struct {
	params arimaParams
}

// NewForecastConfig validates a forecast configuration. horizon must be in
// (0, 168h] and confidence in (0, 1). model holds the
// ARIMAForecastModelKeys parameters of an ARIMA FitProfile, validated as for a
// FitProfile; nil selects the order automatically. History is downsampled to
// the strategy's default maxSamples.
func NewForecastConfig(horizon time.Duration, confidence float64, model map[string]interface{}) (ForecastConfig, error) {
	keys := make([]string, 0, len(model))
	for key := range model {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !slices.Contains(ARIMAForecastModelKeys, key) {
			return ForecastConfig{}, fmt.Errorf("unknown model parameter %q", key)
		}
	}
	params, err := parseARIMAParameters(model)
	if err != nil {
		return ForecastConfig{}, err
	}
	if horizon <= 0 || horizon > maxARIMAHorizon {
		return ForecastConfig{}, fmt.Errorf("horizon must be positive and at most %s, got %s", maxARIMAHorizon, horizon)
	}
	if !(confidence > 0 && confidence < 1) {
		return ForecastConfig{}, fmt.Errorf("confidence must be greater than 0 and less than 1, got %g", confidence)
	}
	params.horizon = horizon
	params.confidence = confidence
	return ForecastConfig{params: params}, nil
}

// Forecast fits a model to history and forecasts it over the configured
// horizon at the configured confidence, with the same preparation, model
// selection and sanity checks as Calculate. Values are in the unit of history
// and clamped at zero. An error means the history cannot be forecast and
// describes why; it wraps ctx.Err() if ctx is done.
func (s *ARIMAStrategy) Forecast(ctx context.Context, history []prometheus.TimeSeriesPoint, cfg ForecastConfig) (fc Forecast, err error) {
	m, err := s.fit(ctx, history, cfg.params)
	if err != nil {
		return fc, err
	}
	defer recoverFit(&err)
	point, lower, upper, err := m.predict(m.steps, cfg.params.confidence)
	if err != nil {
		return fc, err
	}
	if len(point) != len(upper) || len(lower) != len(upper) {
		return fc, errors.New("model returned an incomplete forecast")
	}
	for _, values := range [][]float64{point, lower} {
		for _, v := range values {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return fc, errors.New("model returned a non-finite forecast")
			}
		}
	}
	if _, err := upperBound(upper, m.peak); err != nil {
		return fc, err
	}

	step := int64(m.interval / time.Second)
	fc = Forecast{Model: m.model, SampleInterval: m.interval, Samples: m.samples, Points: make([]ForecastPoint, len(point))}
	for i := range point {
		// The library's approximate normal quantile can cross the bounds by a
		// hair at very low confidence levels.
		v := math.Max(point[i], 0)
		fc.Points[i] = ForecastPoint{
			Timestamp: m.start + int64(i)*step,
			Value:     v,
			Lower:     math.Max(math.Min(lower[i], v), 0),
			Upper:     math.Max(upper[i], v),
		}
	}
	return fc, nil
}
