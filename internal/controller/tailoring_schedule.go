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

package controller

import (
	"hash/fnv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
)

// maxStartupSpread bounds the window over which first analyses (new
// Tailorings, or overdue ones after a controller start) are spread.
const maxStartupSpread = 10 * time.Minute

// spreadOffset returns the Tailoring's stable offset in [0, 1), used to spread
// analyses of many Tailorings over time instead of querying all at once.
func (r *TailoringReconciler) spreadOffset(t *autoscalingv1alpha1.Tailoring) float64 {
	if r.jitter != nil {
		return r.jitter(t)
	}
	key := string(t.UID)
	if key == "" {
		key = t.Namespace + "/" + t.Name
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return float64(h.Sum64()%10000) / 10000
}

func scale(d time.Duration, f float64) time.Duration {
	return time.Duration(float64(d) * f)
}

// startupSpread is the window for first analyses: a tenth of the interval, at
// most maxStartupSpread.
func (r *TailoringReconciler) startupSpread() time.Duration {
	return min(r.analysisInterval()/10, maxStartupSpread)
}

// failureBackoff is the wait after the n-th consecutive failure:
// DefaultRequeueInterval doubled per failure, capped at the analysis interval.
func (r *TailoringReconciler) failureBackoff(failures int32) time.Duration {
	interval := r.analysisInterval()
	if failures < 1 {
		failures = 1
	}
	if failures > 20 {
		return interval
	}
	return min(DefaultRequeueInterval<<(failures-1), interval)
}

// nextAnalysisAt returns when the Tailoring is due for its next analysis.
// Spec changes do not make it due earlier: they are picked up by the next
// scheduled analysis, so edits never cause extra queries.
func (r *TailoringReconciler) nextAnalysisAt(t *autoscalingv1alpha1.Tailoring) time.Time {
	o := r.spreadOffset(t)
	var due time.Time
	switch last := t.Status.LastAnalysis; {
	case t.Status.ConsecutiveFailures > 0 && t.Status.NextAnalysisTime != nil:
		due = t.Status.NextAnalysisTime.Time
	case last != nil && !last.Timestamp.IsZero():
		due = last.Timestamp.Add(r.analysisInterval() + scale(r.analysisInterval()/10, o))
	default:
		due = t.CreationTimestamp.Add(scale(r.startupSpread(), o))
	}
	// Analyses that fell due while the controller was down are spread after
	// its start instead of all running at once.
	if !r.startedAt.IsZero() && due.Before(r.startedAt) {
		due = r.startedAt.Add(scale(r.startupSpread(), o))
	}
	return due
}

// analysisDueIn returns how long until the Tailoring needs a new analysis;
// zero means now.
func (r *TailoringReconciler) analysisDueIn(t *autoscalingv1alpha1.Tailoring) time.Duration {
	remaining := r.nextAnalysisAt(t).Sub(r.clock())
	if remaining <= 0 {
		return 0
	}
	if limit := r.analysisInterval() + r.analysisInterval()/10; remaining > limit {
		// Timestamp in the future (clock skew): wait at most one interval.
		return r.analysisInterval()
	}
	return remaining
}

// scheduleRetry records a failed analysis and returns the backoff until the
// next attempt.
func (r *TailoringReconciler) scheduleRetry(t *autoscalingv1alpha1.Tailoring) time.Duration {
	t.Status.ConsecutiveFailures++
	backoff := r.failureBackoff(t.Status.ConsecutiveFailures)
	wait := backoff + scale(backoff/10, r.spreadOffset(t))
	next := r.clock().Add(wait)
	t.Status.NextAnalysisTime = &metav1.Time{Time: next}
	return wait
}
