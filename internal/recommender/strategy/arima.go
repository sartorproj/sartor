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
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/sartorproj/goarima/arima"
	"github.com/sartorproj/goarima/autoarima"
	"github.com/sartorproj/goarima/sarima"
	"github.com/sartorproj/goarima/timeseries"
	"k8s.io/apimachinery/pkg/api/resource"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/metrics/prometheus"
)

const (
	arimaModeAuto  = "auto"
	arimaModeFixed = "fixed"

	// DefaultARIMASampleInterval is the default history resolution. It
	// matches the 5m resolution of the percentile queries.
	DefaultARIMASampleInterval = 5 * time.Minute

	defaultARIMAConfidence       = 0.95
	defaultARIMALimitsConfidence = 0.99
	defaultARIMAHorizon          = arimaDay
	defaultARIMAMaxSamples       = 1000

	minARIMAConfidence = 0.5
	maxARIMAConfidence = 0.999
	maxARIMAHorizon    = 7 * 24 * time.Hour
	minARIMAMaxSamples = 100
	maxARIMAMaxSamples = 5000

	// Search space of the automatic order selection. Together with
	// maxSamples this bounds the cost of one fit to a few dozen candidate
	// models of at most a few thousand samples each.
	arimaAutoMaxP  = 3
	arimaAutoMaxD  = 1
	arimaAutoMaxQ  = 3
	arimaAutoMaxSP = 1
	arimaAutoMaxSD = 1
	arimaAutoMaxSQ = 1

	// Bounds of fixed orders, matching the library's default search space.
	arimaFixedMaxP  = 5
	arimaFixedMaxD  = 2
	arimaFixedMaxQ  = 5
	arimaFixedMaxSP = 2
	arimaFixedMaxSD = 1
	arimaFixedMaxSQ = 2

	// arimaMinSamples is the shortest series (after downsampling) a model
	// is fitted to.
	arimaMinSamples = 48
	// arimaMaxGapRatio is the largest share of missing samples that is
	// interpolated before the history is considered unusable.
	arimaMaxGapRatio = 0.2
	// arimaMaxGrowth rejects forecasts whose upper bound exceeds the
	// observed peak by more than this factor as a diverging fit.
	arimaMaxGrowth = 10
	// arimaMaxValue keeps forecasts within the range of a resource.Quantity
	// after buffers are applied.
	arimaMaxValue = 1e15
	// arimaMinSeasonalPeriod is the shortest seasonal period in samples.
	arimaMinSeasonalPeriod = 4

	arimaMetaFallback = "_fallback"

	arimaDay  = 24 * time.Hour
	arimaWeek = 7 * arimaDay
)

// Post-processing parameters shared with the percentile strategy.
const (
	paramRequestsBufferPercent = "requestsBufferPercent"
	paramLimitsBufferPercent   = "limitsBufferPercent"
	paramMaxChangePercent      = "maxChangePercent"
)

var arimaPostProcessingKeys = []string{paramRequestsBufferPercent, paramLimitsBufferPercent, paramMaxChangePercent}

// Parameters that select the model.
const (
	paramMode           = "mode"
	paramSeasonal       = "seasonal"
	paramOrder          = "order"
	paramSeasonalOrder  = "seasonalOrder"
	paramSeasonalPeriod = "seasonalPeriod"
)

var arimaParameterKeys = map[string]bool{
	paramMode: true, paramOrder: true, paramSeasonalOrder: true, paramSeasonalPeriod: true, paramSeasonal: true,
	"confidence": true, "limitsConfidence": true, "horizon": true, "maxSamples": true,
	paramRequestsBufferPercent: true, paramLimitsBufferPercent: true, paramMaxChangePercent: true,
}

// ARIMAStrategy forecasts CPU and memory usage with (seasonal) ARIMA models
// and recommends the upper bound of the prediction interval over a horizon.
// Resources whose history cannot be modelled fall back to the percentile
// strategy; the reason is recorded in the recommendation metadata.
type ARIMAStrategy struct {
	sampleInterval time.Duration
	percentile     *PercentileStrategy
}

// NewARIMAStrategy creates an ARIMA strategy that requests usage history at
// sampleInterval resolution (DefaultARIMASampleInterval if not positive).
func NewARIMAStrategy(sampleInterval time.Duration) *ARIMAStrategy {
	if sampleInterval <= 0 {
		sampleInterval = DefaultARIMASampleInterval
	}
	return &ARIMAStrategy{sampleInterval: sampleInterval, percentile: NewPercentileStrategy()}
}

// Name returns the strategy name.
func (s *ARIMAStrategy) Name() string {
	return StrategyNameARIMA
}

// HistoryStep returns the sample interval, coarsened for long windows so the
// history stays within prometheus.MaxHistoryPoints samples.
func (s *ARIMAStrategy) HistoryStep(window time.Duration) time.Duration {
	step := s.sampleInterval
	if minStep := window / prometheus.MaxHistoryPoints; step < minStep {
		step = minStep.Truncate(time.Second) + time.Second
	}
	return step
}

// arimaParams holds the parsed FitProfile parameters.
type arimaParams struct {
	mode             string
	order            [3]int // p, d, q
	seasonalOrder    [3]int // P, D, Q
	seasonalPeriod   time.Duration
	seasonal         bool
	confidence       float64
	limitsConfidence float64
	horizon          time.Duration
	maxSamples       int
}

// arimaForecast is the forecast of one resource.
type arimaForecast struct {
	model    string
	interval time.Duration
	samples  int
	// requests and limits are the largest upper bounds over the horizon at
	// the requests and limits confidence levels.
	requests float64
	limits   float64
	// start is the timestamp of the first forecast step.
	start int64
	point []float64
	upper []float64
}

// Calculate computes recommendations from the forecast of each container's
// usage history, using the percentile strategy's buffers, gradual change,
// safety rails and quota handling.
func (s *ARIMAStrategy) Calculate(
	ctx context.Context,
	metrics []prometheus.ContainerMetrics,
	currentResources map[string]autoscalingv1alpha1.ResourceRequirements,
	config StrategyConfig,
) ([]Recommendation, error) {
	params, err := parseARIMAParameters(config.Parameters)
	if err != nil {
		return nil, fmt.Errorf("invalid arima parameters: %w", err)
	}
	intentConfig := s.percentile.getIntentConfigFromParameters(config)

	recommendations := make([]Recommendation, 0, len(metrics))
	for _, m := range metrics {
		recommendations = append(recommendations,
			s.calculateForContainer(ctx, m, currentResources[m.ContainerName], intentConfig, config, params))
	}
	return recommendations, nil
}

// calculateForContainer replaces the P95/P99 inputs of the percentile
// calculation with the forecast upper bounds of every resource that could be
// modelled, so both paths share the same post-processing.
func (s *ARIMAStrategy) calculateForContainer(
	ctx context.Context,
	metrics prometheus.ContainerMetrics,
	current autoscalingv1alpha1.ResourceRequirements,
	intentConfig IntentConfig,
	config StrategyConfig,
	params arimaParams,
) Recommendation {
	cpu, cpuErr := s.forecast(ctx, metrics.CPUHistory, params)
	mem, memErr := s.forecast(ctx, metrics.MemoryHistory, params)

	input := metrics
	if cpuErr == nil {
		input.P95CPU = cpuQuantity(cpu.requests)
		input.P99CPU = cpuQuantity(cpu.limits)
	}
	if memErr == nil {
		input.P95Memory = memoryQuantity(mem.requests)
		input.P99Memory = memoryQuantity(mem.limits)
	}

	rec := s.percentile.calculateForContainer(input, current, intentConfig, config)
	rec.Method = StrategyNameARIMA
	rec.Metadata["horizon"] = params.horizon.String()
	if cpuErr == nil || memErr == nil {
		rec.Confidence = params.confidence
	}
	for _, r := range []struct {
		name string
		f    arimaForecast
		err  error
		q    func(float64) *resource.Quantity
	}{
		{resourceTypeCPU, cpu, cpuErr, cpuQuantity},
		{resourceTypeMemory, mem, memErr, memoryQuantity},
	} {
		if r.err != nil {
			rec.Metadata[r.name+arimaMetaFallback] = "percentile: " + r.err.Error()
			rec.Confidence = math.Min(rec.Confidence, 0.8)
			continue
		}
		delete(rec.Metadata, r.name+"_p95")
		delete(rec.Metadata, r.name+"_p99")
		rec.Metadata[r.name+"_model"] = r.f.model
		rec.Metadata[r.name+"_sample_interval"] = r.f.interval.String()
		rec.Metadata[r.name+"_samples"] = r.f.samples
		rec.Metadata[r.name+"_upper_bound"] = r.q(r.f.requests).String()
		rec.Metadata[r.name+"_limits_upper_bound"] = r.q(r.f.limits).String()
	}
	return rec
}

// forecast fits a model to history and returns the upper bounds of its
// prediction intervals over the horizon. Any error means the resource falls
// back to the percentile calculation.
func (s *ARIMAStrategy) forecast(ctx context.Context, history []prometheus.TimeSeriesPoint, params arimaParams) (f arimaForecast, err error) {
	m, err := s.fit(ctx, history, params)
	if err != nil {
		return f, err
	}
	defer recoverFit(&err)
	point, _, upper, err := m.predict(m.steps, params.confidence)
	if err != nil {
		return f, err
	}
	_, _, limitsUpper, err := m.predict(m.steps, params.limitsConfidence)
	if err != nil {
		return f, err
	}

	f = arimaForecast{
		model:    m.model,
		interval: m.interval,
		samples:  m.samples,
		start:    m.start,
		point:    point,
		upper:    upper,
	}
	if f.requests, err = upperBound(upper, m.peak); err != nil {
		return f, err
	}
	if f.limits, err = upperBound(limitsUpper, m.peak); err != nil {
		return f, err
	}
	f.limits = math.Max(f.limits, f.requests)
	return f, nil
}

// arimaFit is a model fitted to a prepared usage history.
type arimaFit struct {
	predict  predictFunc
	model    string
	interval time.Duration
	samples  int
	// start is the timestamp of the first forecast step.
	start int64
	// steps is the number of forecast steps that cover the horizon.
	steps int
	// peak is the largest value of the prepared history.
	peak float64
}

// fit prepares history and fits the configured model to it. Any error means
// the history cannot be forecast.
func (s *ARIMAStrategy) fit(ctx context.Context, history []prometheus.TimeSeriesPoint, params arimaParams) (f arimaFit, err error) {
	if err := ctx.Err(); err != nil {
		return f, fmt.Errorf("analysis cancelled: %w", err)
	}
	if len(history) == 0 {
		return f, errors.New("no usage history")
	}
	values, start, interval, err := s.prepareSeries(history, params.maxSamples)
	if err != nil {
		return f, err
	}
	if span := time.Duration(len(values)) * interval; span < params.horizon {
		return f, fmt.Errorf("history too short: %s covers less than the %s horizon", span, params.horizon)
	}
	lo, hi := minMax(values)
	if hi-lo <= 1e-9*math.Max(math.Abs(hi), 1e-12) {
		return f, errors.New("usage history is constant")
	}

	// The library is not hardened against every degenerate input; a panic
	// in a fit must not take down the analysis of other workloads.
	defer recoverFit(&err)

	predict, model, err := fitARIMA(timeseries.New(values), interval, params)
	if err != nil {
		return f, err
	}
	return arimaFit{
		predict:  predict,
		model:    model,
		interval: interval,
		samples:  len(values),
		start:    start + int64(len(values))*int64(interval/time.Second),
		steps:    int(math.Ceil(float64(params.horizon) / float64(interval))),
		peak:     hi,
	}, nil
}

// recoverFit turns a panic in the library into an error. It must be deferred
// directly, so that recover takes effect.
func recoverFit(err *error) {
	if r := recover(); r != nil {
		*err = fmt.Errorf("model fit failed: %v", r)
	}
}

// predictFunc returns the point forecast and the lower and upper prediction
// bounds at a confidence level for the given number of steps.
type predictFunc func(steps int, confidence float64) (point, lower, upper []float64, err error)

// fitARIMA fits the configured model and returns its predictor and order.
func fitARIMA(series *timeseries.Series, interval time.Duration, params arimaParams) (predictFunc, string, error) {
	if params.mode == arimaModeFixed {
		return fitFixedARIMA(series, interval, params)
	}

	cfg := autoarima.DefaultConfig()
	cfg.MaxP, cfg.MaxD, cfg.MaxQ = arimaAutoMaxP, arimaAutoMaxD, arimaAutoMaxQ
	cfg.MaxSP, cfg.MaxSD, cfg.MaxSQ = arimaAutoMaxSP, arimaAutoMaxSD, arimaAutoMaxSQ
	cfg.AutoSeasonal = false
	if params.seasonal {
		if periods := seasonalPeriods(interval, series.Len()); len(periods) > 0 {
			cfg.AutoSeasonal = true
			cfg.SeasonalPeriods = periods
			cfg.MinSeasonalPeriod = periods[0]
			cfg.MaxSeasonalPeriod = periods[len(periods)-1]
		}
	}
	result, err := autoarima.AutoARIMA(series, cfg)
	if err != nil {
		return nil, "", fmt.Errorf("model selection failed: %w", err)
	}
	if result == nil {
		return nil, "", errors.New("model selection found no model that fits")
	}
	return result.PredictWithInterval, result.Order(), nil
}

// fitFixedARIMA fits the ARIMA or SARIMA model of the configured orders.
func fitFixedARIMA(series *timeseries.Series, interval time.Duration, params arimaParams) (predictFunc, string, error) {
	p, d, q := params.order[0], params.order[1], params.order[2]
	sp, sd, sq := params.seasonalOrder[0], params.seasonalOrder[1], params.seasonalOrder[2]
	if sp == 0 && sd == 0 && sq == 0 {
		model := arima.New(p, d, q)
		if err := model.Fit(series); err != nil {
			return nil, "", fmt.Errorf("model fit failed: %w", err)
		}
		return model.PredictWithInterval, fmt.Sprintf("ARIMA(%d,%d,%d)", p, d, q), nil
	}

	m := int(math.Round(float64(params.seasonalPeriod) / float64(interval)))
	if m < 2 {
		return nil, "", fmt.Errorf("seasonal period %s is shorter than two %s samples", params.seasonalPeriod, interval)
	}
	model := sarima.New(p, d, q, sp, sd, sq, m)
	if err := model.Fit(series); err != nil {
		return nil, "", fmt.Errorf("model fit failed: %w", err)
	}
	return model.PredictWithInterval, fmt.Sprintf("SARIMA(%d,%d,%d)(%d,%d,%d)[%d]", p, d, q, sp, sd, sq, m), nil
}

// seasonalPeriods returns the daily and weekly periods in samples that a
// series of n samples at interval can reveal. Seasonality detection needs
// three cycles, and a period must be within 5% of a whole number of samples.
func seasonalPeriods(interval time.Duration, n int) []int {
	var periods []int
	for _, cycle := range []time.Duration{arimaDay, arimaWeek} {
		exact := float64(cycle) / float64(interval)
		period := int(math.Round(exact))
		if period >= arimaMinSeasonalPeriod && 3*period <= n && math.Abs(float64(period)-exact) <= 0.05*exact {
			periods = append(periods, period)
		}
	}
	return periods
}

// upperBound returns the largest finite, non-negative value of upper and
// rejects intervals that are not finite or diverge from the observed peak.
func upperBound(upper []float64, peak float64) (float64, error) {
	if len(upper) == 0 {
		return 0, errors.New("model returned no forecast")
	}
	bound := math.Inf(-1)
	for _, v := range upper {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, errors.New("model returned a non-finite forecast")
		}
		bound = math.Max(bound, v)
	}
	switch {
	case bound <= 0:
		return 0, errors.New("model forecasts no usage")
	case bound > arimaMaxValue || bound > peak*arimaMaxGrowth:
		return 0, fmt.Errorf("forecast %.4g diverges from the observed peak %.4g", bound, peak)
	}
	return bound, nil
}

// prepareSeries turns raw history into a regular series of at most
// maxSamples values. Samples that are not finite or negative are dropped,
// samples are aggregated with max into buckets of a common interval
// (preserving peaks), and short gaps are filled by linear interpolation.
// It returns the values, the timestamp of the first bucket and the interval.
func (s *ARIMAStrategy) prepareSeries(history []prometheus.TimeSeriesPoint, maxSamples int) ([]float64, int64, time.Duration, error) {
	points := make([]prometheus.TimeSeriesPoint, 0, len(history))
	for _, p := range history {
		if !math.IsNaN(p.Value) && !math.IsInf(p.Value, 0) && p.Value >= 0 {
			points = append(points, p)
		}
	}
	if len(points) < arimaMinSamples {
		return nil, 0, 0, fmt.Errorf("history too short: %d valid samples, need %d", len(points), arimaMinSamples)
	}
	sort.SliceStable(points, func(i, j int) bool { return points[i].Timestamp < points[j].Timestamp })

	diffs := make([]int64, 0, len(points)-1)
	for i := 1; i < len(points); i++ {
		if d := points[i].Timestamp - points[i-1].Timestamp; d > 0 {
			diffs = append(diffs, d)
		}
	}
	if len(diffs) == 0 {
		return nil, 0, 0, errors.New("history too short: all samples share one timestamp")
	}
	sort.Slice(diffs, func(i, j int) bool { return diffs[i] < diffs[j] })
	base := max(int64(s.sampleInterval/time.Second), diffs[len(diffs)/2], 1)

	first, last := points[0].Timestamp, points[len(points)-1].Timestamp
	if last-first <= 0 {
		return nil, 0, 0, errors.New("history timestamps are out of range")
	}
	factor := max((last-first)/base/int64(maxSamples)+1, 1)
	// Prefer an interval that divides a day so seasonal periods are whole.
	daySeconds := int64(arimaDay / time.Second)
	for f := factor; f <= 2*factor && base*f <= daySeconds; f++ {
		if daySeconds%(base*f) == 0 {
			factor = f
			break
		}
	}
	interval := base * factor

	n := int((last-first)/interval) + 1
	values := make([]float64, n)
	filled := make([]bool, n)
	for _, p := range points {
		i := int((p.Timestamp - first) / interval)
		if !filled[i] || p.Value > values[i] {
			values[i] = p.Value
		}
		filled[i] = true
	}
	if n < arimaMinSamples {
		return nil, 0, 0, fmt.Errorf("history too short: %d samples at %s, need %d", n, time.Duration(interval)*time.Second, arimaMinSamples)
	}

	missing := 0
	for _, ok := range filled {
		if !ok {
			missing++
		}
	}
	if ratio := float64(missing) / float64(n); ratio > arimaMaxGapRatio {
		return nil, 0, 0, fmt.Errorf("history has too many gaps: %.0f%% of samples missing", ratio*100)
	}
	interpolateGaps(values, filled)
	return values, first, time.Duration(interval) * time.Second, nil
}

// interpolateGaps fills unfilled values linearly between their filled
// neighbours. The first and last values must be filled.
func interpolateGaps(values []float64, filled []bool) {
	prev := 0
	for i := 1; i < len(values); i++ {
		if !filled[i] {
			continue
		}
		for j := prev + 1; j < i; j++ {
			frac := float64(j-prev) / float64(i-prev)
			values[j] = values[prev] + frac*(values[i]-values[prev])
		}
		prev = i
	}
}

func minMax(values []float64) (lo, hi float64) {
	lo, hi = values[0], values[0]
	for _, v := range values[1:] {
		lo, hi = math.Min(lo, v), math.Max(hi, v)
	}
	return lo, hi
}

// cpuQuantity converts cores to a quantity, rounding up to whole millicores.
func cpuQuantity(cores float64) *resource.Quantity {
	return resource.NewMilliQuantity(int64(math.Ceil(cores*1000)), resource.DecimalSI)
}

// memoryQuantity converts bytes to a quantity, rounding up to whole bytes.
func memoryQuantity(bytes float64) *resource.Quantity {
	return resource.NewQuantity(int64(math.Ceil(bytes)), resource.BinarySI)
}

// parseARIMAParameters validates FitProfile parameters and applies defaults.
func parseARIMAParameters(parameters map[string]interface{}) (arimaParams, error) {
	p := arimaParams{
		mode:             arimaModeAuto,
		seasonal:         true,
		confidence:       defaultARIMAConfidence,
		limitsConfidence: defaultARIMALimitsConfidence,
		horizon:          defaultARIMAHorizon,
		maxSamples:       defaultARIMAMaxSamples,
	}

	keys := make([]string, 0, len(parameters))
	for key := range parameters {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !arimaParameterKeys[key] {
			return p, fmt.Errorf("unknown parameter %q", key)
		}
	}

	if v, ok := parameters["mode"]; ok {
		mode, isString := v.(string)
		if !isString || (mode != arimaModeAuto && mode != arimaModeFixed) {
			return p, fmt.Errorf("mode must be '%s' or '%s', got %v", arimaModeAuto, arimaModeFixed, v)
		}
		p.mode = mode
	}

	if err := parseARIMAForecastParameters(parameters, &p); err != nil {
		return p, err
	}
	if p.mode == arimaModeAuto {
		return p, parseARIMAAutoParameters(parameters, &p)
	}
	return p, parseARIMAFixedParameters(parameters, &p)
}

// parseARIMAForecastParameters parses the parameters shared by both modes.
func parseARIMAForecastParameters(parameters map[string]interface{}, p *arimaParams) error {
	var err error
	if p.confidence, err = numberParam(parameters, "confidence", p.confidence, minARIMAConfidence, maxARIMAConfidence); err != nil {
		return err
	}
	if p.limitsConfidence, err = numberParam(parameters, "limitsConfidence", p.limitsConfidence, minARIMAConfidence, maxARIMAConfidence); err != nil {
		return err
	}
	if p.limitsConfidence < p.confidence {
		return fmt.Errorf("limitsConfidence (%g) must be >= confidence (%g)", p.limitsConfidence, p.confidence)
	}
	if p.horizon, err = durationParam(parameters, "horizon", p.horizon, maxARIMAHorizon); err != nil {
		return err
	}
	maxSamples, err := numberParam(parameters, "maxSamples", float64(p.maxSamples), minARIMAMaxSamples, maxARIMAMaxSamples)
	if err != nil {
		return err
	}
	if maxSamples != math.Trunc(maxSamples) {
		return fmt.Errorf("maxSamples must be a whole number, got %g", maxSamples)
	}
	p.maxSamples = int(maxSamples)
	for _, key := range arimaPostProcessingKeys {
		if _, err := numberParam(parameters, key, 0, 0, 1000); err != nil {
			return err
		}
	}
	return nil
}

// parseARIMAAutoParameters parses the parameters of mode 'auto'.
func parseARIMAAutoParameters(parameters map[string]interface{}, p *arimaParams) error {
	for _, key := range []string{"order", "seasonalOrder", "seasonalPeriod"} {
		if _, ok := parameters[key]; ok {
			return fmt.Errorf("%s requires mode '%s'", key, arimaModeFixed)
		}
	}
	if v, ok := parameters["seasonal"]; ok {
		seasonal, isBool := v.(bool)
		if !isBool {
			return fmt.Errorf("seasonal must be a boolean, got %v", v)
		}
		p.seasonal = seasonal
	}
	return nil
}

// parseARIMAFixedParameters parses the parameters of mode 'fixed'.
func parseARIMAFixedParameters(parameters map[string]interface{}, p *arimaParams) error {
	if _, ok := parameters["seasonal"]; ok {
		return fmt.Errorf("seasonal requires mode '%s'; use seasonalOrder in mode '%s'", arimaModeAuto, arimaModeFixed)
	}
	if _, ok := parameters["order"]; !ok {
		return fmt.Errorf("mode '%s' requires order [p, d, q]", arimaModeFixed)
	}
	var err error
	if p.order, err = orderParam(parameters["order"], "order", [3]int{arimaFixedMaxP, arimaFixedMaxD, arimaFixedMaxQ}); err != nil {
		return err
	}
	if v, ok := parameters["seasonalOrder"]; ok {
		if p.seasonalOrder, err = orderParam(v, "seasonalOrder", [3]int{arimaFixedMaxSP, arimaFixedMaxSD, arimaFixedMaxSQ}); err != nil {
			return err
		}
	}
	seasonal := p.seasonalOrder != [3]int{}
	_, hasPeriod := parameters["seasonalPeriod"]
	switch {
	case seasonal && !hasPeriod:
		return errors.New("seasonalOrder requires seasonalPeriod")
	case !seasonal && hasPeriod:
		return errors.New("seasonalPeriod requires a non-zero seasonalOrder")
	case seasonal:
		if p.seasonalPeriod, err = durationParam(parameters, "seasonalPeriod", 0, arimaWeek); err != nil {
			return err
		}
	}
	return nil
}

// numberParam returns the numeric parameter key within [lo, hi], or def if
// it is absent.
func numberParam(parameters map[string]interface{}, key string, def, lo, hi float64) (float64, error) {
	v, ok := parameters[key]
	if !ok {
		return def, nil
	}
	n, ok := toFloat(v)
	if !ok {
		return 0, fmt.Errorf("%s must be a number, got %v", key, v)
	}
	if math.IsNaN(n) || n < lo || n > hi {
		return 0, fmt.Errorf("%s must be between %g and %g, got %g", key, lo, hi, n)
	}
	return n, nil
}

// durationParam returns the duration parameter key within (0, hi], or def
// if it is absent.
func durationParam(parameters map[string]interface{}, key string, def, hi time.Duration) (time.Duration, error) {
	v, ok := parameters[key]
	if !ok {
		return def, nil
	}
	str, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("%s must be a duration string such as \"24h\", got %v", key, v)
	}
	d, err := time.ParseDuration(str)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if d <= 0 || d > hi {
		return 0, fmt.Errorf("%s must be positive and at most %s, got %s", key, hi, d)
	}
	return d, nil
}

// orderParam parses a model order of three whole numbers bounded by limits.
func orderParam(v interface{}, key string, limits [3]int) ([3]int, error) {
	var order [3]int
	var items []interface{}
	switch t := v.(type) {
	case []interface{}:
		items = t
	case []int:
		for _, i := range t {
			items = append(items, i)
		}
	case []float64:
		for _, f := range t {
			items = append(items, f)
		}
	default:
		return order, fmt.Errorf("%s must be an array of 3 integers, got %v", key, v)
	}
	if len(items) != len(order) {
		return order, fmt.Errorf("%s must have 3 elements, got %d", key, len(items))
	}
	for i, item := range items {
		n, ok := toFloat(item)
		if !ok || n != math.Trunc(n) || n < 0 || n > float64(limits[i]) {
			return order, fmt.Errorf("%s[%d] must be an integer between 0 and %d, got %v", key, i, limits[i], item)
		}
		order[i] = int(n)
	}
	return order, nil
}

func toFloat(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// ValidateConfig validates the strategy configuration.
func (s *ARIMAStrategy) ValidateConfig(config StrategyConfig) error {
	_, err := parseARIMAParameters(config.Parameters)
	return err
}

// ValidateFitProfileParameters validates FitProfile parameters for the ARIMA strategy.
func (s *ARIMAStrategy) ValidateFitProfileParameters(parameters map[string]interface{}) error {
	_, err := parseARIMAParameters(parameters)
	return err
}

// GetDefaultFitProfileSpec returns the default FitProfile spec for the ARIMA strategy.
func (s *ARIMAStrategy) GetDefaultFitProfileSpec() FitProfileSpec {
	exampleParams := map[string]interface{}{
		"mode":                     arimaModeAuto,
		"seasonal":                 true,
		"confidence":               defaultARIMAConfidence,
		"limitsConfidence":         defaultARIMALimitsConfidence,
		"horizon":                  "24h",
		"maxSamples":               defaultARIMAMaxSamples,
		paramRequestsBufferPercent: 20.0,
		paramLimitsBufferPercent:   30.0,
		paramMaxChangePercent:      50.0,
	}

	var paramsSchema map[string]interface{}
	_ = json.Unmarshal([]byte(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"mode": {
				"type": "string",
				"enum": ["auto", "fixed"],
				"description": "'auto' selects the model order automatically, 'fixed' uses order and seasonalOrder",
				"default": "auto"
			},
			"seasonal": {
				"type": "boolean",
				"description": "Mode 'auto' only: also consider seasonal models with daily and weekly periods",
				"default": true
			},
			"order": {
				"type": "array",
				"items": {"type": "integer", "minimum": 0},
				"minItems": 3,
				"maxItems": 3,
				"description": "Mode 'fixed' only (required): non-seasonal order [p, d, q], p,q <= 5, d <= 2"
			},
			"seasonalOrder": {
				"type": "array",
				"items": {"type": "integer", "minimum": 0},
				"minItems": 3,
				"maxItems": 3,
				"description": "Mode 'fixed' only: seasonal order [P, D, Q], P,Q <= 2, D <= 1",
				"default": [0, 0, 0]
			},
			"seasonalPeriod": {
				"type": "string",
				"description": "Mode 'fixed' only: seasonal period s as a duration (e.g. '24h'), required with a non-zero seasonalOrder"
			},
			"confidence": {
				"type": "number",
				"description": "Prediction interval level whose upper bound sets requests",
				"minimum": 0.5,
				"maximum": 0.999,
				"default": 0.95
			},
			"limitsConfidence": {
				"type": "number",
				"description": "Prediction interval level whose upper bound sets limits (>= confidence)",
				"minimum": 0.5,
				"maximum": 0.999,
				"default": 0.99
			},
			"horizon": {
				"type": "string",
				"description": "Forecast horizon as a duration, at most 168h",
				"default": "24h"
			},
			"maxSamples": {
				"type": "integer",
				"description": "History is downsampled (max per bucket) to at most this many samples",
				"minimum": 100,
				"maximum": 5000,
				"default": 1000
			},
			"requestsBufferPercent": {
				"type": "number",
				"description": "Buffer percentage added to the requests upper bound",
				"minimum": 0,
				"maximum": 1000,
				"default": 20
			},
			"limitsBufferPercent": {
				"type": "number",
				"description": "Buffer percentage added to the limits upper bound",
				"minimum": 0,
				"maximum": 1000,
				"default": 30
			},
			"maxChangePercent": {
				"type": "number",
				"description": "Maximum change percentage per recommendation",
				"minimum": 0,
				"maximum": 1000,
				"default": 50
			}
		}
	}`), &paramsSchema)

	return FitProfileSpec{
		Strategy:          StrategyNameARIMA,
		DisplayName:       "ARIMA Forecasting",
		Description:       "Forecasts usage with automatically selected (seasonal) ARIMA models and sizes for the upper prediction bound; falls back to percentiles",
		ParametersSchema:  paramsSchema,
		ExampleParameters: exampleParams,
	}
}

// GetChartLines returns the point forecast and the requests upper bound of
// each forecast resource over the horizon.
func (s *ARIMAStrategy) GetChartLines(
	ctx context.Context,
	metrics []prometheus.ContainerMetrics,
	_ map[string]autoscalingv1alpha1.ResourceRequirements,
	recommendations []Recommendation,
	config StrategyConfig,
) (map[string]map[int64]float64, error) {
	params, err := parseARIMAParameters(config.Parameters)
	if err != nil {
		return nil, fmt.Errorf("invalid arima parameters: %w", err)
	}
	lines := make(map[string]map[int64]float64)
	for _, rec := range recommendations {
		m := findContainerMetrics(metrics, rec.ContainerName)
		if m == nil {
			continue
		}
		for name, history := range map[string][]prometheus.TimeSeriesPoint{
			resourceTypeCPU:    m.CPUHistory,
			resourceTypeMemory: m.MemoryHistory,
		} {
			f, err := s.forecast(ctx, history, params)
			if err != nil {
				continue
			}
			step := int64(f.interval / time.Second)
			for _, line := range []struct {
				key    string
				values []float64
			}{{name + "_forecast", f.point}, {name + "_forecast_upper", f.upper}} {
				if lines[line.key] == nil {
					lines[line.key] = make(map[int64]float64)
				}
				for i, v := range line.values {
					lines[line.key][f.start+int64(i)*step] = math.Max(v, 0)
				}
			}
		}
	}
	return lines, nil
}
