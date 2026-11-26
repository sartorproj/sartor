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

package preprocessing

import (
	"math"
	"testing"
)

func series(values ...float64) []TimeSeries {
	out := make([]TimeSeries, len(values))
	for i, v := range values {
		out[i] = TimeSeries{Timestamp: int64(i * 60), Value: v}
	}
	return out
}

func TestPercentile(t *testing.T) {
	values := []float64{5, 1, 4, 2, 3}
	tests := []struct {
		p    float64
		want float64
	}{
		{p: 0, want: 1},
		{p: 25, want: 2},
		{p: 50, want: 3},
		{p: 62.5, want: 3.5},
		{p: 100, want: 5},
		{p: 99.9, want: 4.996},
		{p: -10, want: 1},
		{p: 150, want: 5},
	}
	for _, tt := range tests {
		if got := percentile(values, tt.p); math.Abs(got-tt.want) > 1e-9 {
			t.Errorf("percentile(%v) = %v, want %v", tt.p, got, tt.want)
		}
	}
	if got := percentile(nil, 50); got != 0 {
		t.Errorf("percentile(nil) = %v, want 0", got)
	}
}

func TestRemoveOutliers(t *testing.T) {
	p := NewPreprocessor()
	p.OutlierPercentile = [2]float64{10, 90}

	tests := []struct {
		name string
		in   []TimeSeries
		want []float64
	}{
		{name: "empty", in: nil, want: nil},
		{name: "single point", in: series(7), want: []float64{7}},
		{name: "constant series is unchanged", in: series(2, 2, 2, 2), want: []float64{2, 2, 2, 2}},
		{
			name: "spikes are replaced by the previous value",
			in:   series(1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 100, 1),
			want: []float64{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1},
		},
		{
			name: "leading outlier is replaced by the next value",
			in:   series(-50, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1),
			want: []float64{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractValues(p.RemoveOutliers(tt.in))
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestProcess_DefaultPercentilesDoNotPanic(t *testing.T) {
	p := NewPreprocessor()
	p.EnableNormalization = true
	in := series(1, 2, 3, 4, 5, 6, 7, 8, 9, 10)
	out := p.Process(in)
	if len(out) != len(in) {
		t.Fatalf("Process returned %d points, want %d", len(out), len(in))
	}
	mean := 0.0
	for _, v := range ExtractValues(out) {
		mean += v
	}
	if math.Abs(mean/float64(len(out))) > 1e-9 {
		t.Errorf("normalized mean = %v, want 0", mean/float64(len(out)))
	}
}

func TestNormalize(t *testing.T) {
	p := NewPreprocessor()
	if got := p.Normalize(nil); len(got) != 0 {
		t.Errorf("Normalize(nil) = %v", got)
	}
	constant := series(3, 3, 3)
	if got := ExtractValues(p.Normalize(constant)); got[0] != 3 || got[2] != 3 {
		t.Errorf("constant series should be unchanged, got %v", got)
	}
	got := ExtractValues(p.Normalize(series(1, 3)))
	if got[0] != -1 || got[1] != 1 {
		t.Errorf("Normalize(1,3) = %v, want [-1 1]", got)
	}
}

func TestImputeMissing(t *testing.T) {
	p := NewPreprocessor()
	if got := p.ImputeMissing(series(1)); len(got) != 1 {
		t.Errorf("single point: %v", got)
	}
	in := []TimeSeries{{Timestamp: 0, Value: 1}, {Timestamp: 60, Value: 2}, {Timestamp: 60, Value: 3}}
	if got := p.ImputeMissing(in); len(got) != 3 {
		t.Errorf("ImputeMissing should keep all points, got %v", got)
	}
}

func TestMinMaxNormalization(t *testing.T) {
	if got := MinMaxNormalization(nil); len(got) != 0 {
		t.Errorf("empty = %v", got)
	}
	if got := MinMaxNormalization([]float64{4, 4}); got[0] != 0.5 || got[1] != 0.5 {
		t.Errorf("constant = %v, want [0.5 0.5]", got)
	}
	got := MinMaxNormalization([]float64{2, 4, 6})
	if got[0] != 0 || got[1] != 0.5 || got[2] != 1 {
		t.Errorf("MinMaxNormalization = %v, want [0 0.5 1]", got)
	}
}
