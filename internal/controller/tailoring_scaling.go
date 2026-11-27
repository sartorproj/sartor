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
	"context"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
	"github.com/sartorproj/sartor/internal/gitops/writer"
	"github.com/sartorproj/sartor/internal/metrics/prometheus"
	"github.com/sartorproj/sartor/internal/recommender"
	"github.com/sartorproj/sartor/internal/recommender/scaling"
	"github.com/sartorproj/sartor/internal/recommender/strategy"
)

// ConditionTypeAutoscalerConflict reports autoscalers that act on the same
// resource (for example a VPA and an HPA on cpu). Sartor flags them and
// does not try to resolve them.
const ConditionTypeAutoscalerConflict = "AutoscalerConflict"

// scalingSignalsReader reads the usage and replica statistics of a workload.
type scalingSignalsReader interface {
	GetScalingSignals(ctx context.Context, namespace string, workload prometheus.Workload, window time.Duration) (*prometheus.ScalingSignals, error)
}

// applyScaling adjusts recs to the workload's autoscalers and returns the
// scaling summary for the status. Signals are only read when an autoscaler
// is detected; without them the recommendation stays per pod.
func applyScaling(
	ctx context.Context,
	t *autoscalingv1alpha1.Tailoring,
	signals scalingSignalsReader,
	view *workloadView,
	metrics []prometheus.ContainerMetrics,
	recs []strategy.Recommendation,
	rails recommender.SafetyRails,
	window time.Duration,
	logger logr.Logger,
) *autoscalingv1alpha1.ScalingSummary {
	in := scaling.Input{
		WorkloadKind:    t.Spec.Target.Kind,
		Current:         view.current,
		Metrics:         metrics,
		CurrentReplicas: view.replicas,
		HPAs:            horizontalAutoscalers(view.autoscalers),
		VPAs:            verticalAutoscalers(view.autoscalers, view.vpaPolicies),
		OOMKilled:       view.oomKilled,
		Rails:           scaling.Rails{MinCPU: rails.MinCPU, MaxCPU: rails.MaxCPU, MinMemory: rails.MinMemory, MaxMemory: rails.MaxMemory},
	}
	if len(view.autoscalers) > 0 && signals != nil {
		s, err := signals.GetScalingSignals(ctx, t.TargetNamespace(), targetWorkload(t), window)
		if err != nil {
			logger.Error(err, "Failed to get scaling signals, sizing per pod")
		} else {
			in.Signals = s
		}
	}
	summary := scaling.Apply(recs, in)
	if t.Spec.WriteBack.Type != autoscalingv1alpha1.WriteBackTypeRaw {
		for i := range summary.VPA {
			if summary.VPA[i].WriteBack {
				summary.VPA[i].WriteBack = false
				summary.VPA[i].Reason += " Only raw write-back edits VPA objects; apply the bounds by hand."
			}
		}
	}
	return summary
}

// horizontalAutoscalers returns the HPAs, plus the ScaledObjects whose HPA
// was not detected.
func horizontalAutoscalers(detected []autoscalingv1alpha1.DetectedAutoscaler) []scaling.HPA {
	managed := map[string]bool{}
	for _, a := range detected {
		if a.Kind == autoscalingv1alpha1.AutoscalerKindHPA && a.ManagedBy != "" {
			managed[a.ManagedBy] = true
		}
	}
	var out []scaling.HPA
	for _, a := range detected {
		switch {
		case a.Kind == autoscalingv1alpha1.AutoscalerKindHPA,
			a.Kind == autoscalingv1alpha1.AutoscalerKindScaledObject && !managed[a.Name]:
			out = append(out, scaling.HPA{
				Kind: a.Kind, Name: a.Name, ManagedBy: a.ManagedBy,
				MinReplicas: a.MinReplicas, MaxReplicas: a.MaxReplicas,
				Targets: a.UtilizationTargets, OtherMetrics: a.OtherMetrics,
			})
		}
	}
	return out
}

func verticalAutoscalers(detected []autoscalingv1alpha1.DetectedAutoscaler, policies map[string][]scaling.VPAPolicy) []scaling.VPA {
	var out []scaling.VPA
	for _, a := range detected {
		if a.Kind == autoscalingv1alpha1.AutoscalerKindVPA {
			out = append(out, scaling.VPA{Name: a.Name, UpdateMode: a.UpdateMode, Policies: policies[a.Name]})
		}
	}
	return out
}

// metricsVPAPolicies turns the bounds kube-state-metrics reports into
// container policies; modes and controlled resources are not reported.
func metricsVPAPolicies(v prometheus.VPAInfo) []scaling.VPAPolicy {
	names := map[string]bool{}
	for n := range v.MinAllowed {
		names[n] = true
	}
	for n := range v.MaxAllowed {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	out := make([]scaling.VPAPolicy, 0, len(sorted))
	for _, n := range sorted {
		out = append(out, scaling.VPAPolicy{
			ContainerName: n,
			MinAllowed:    resourceValues(v.MinAllowed[n]),
			MaxAllowed:    resourceValues(v.MaxAllowed[n]),
		})
	}
	return out
}

func resourceValues(v prometheus.ResourceValues) *autoscalingv1alpha1.ResourceValues {
	if v.CPU == nil && v.Memory == nil {
		return nil
	}
	return &autoscalingv1alpha1.ResourceValues{CPU: v.CPU, Memory: v.Memory}
}

// setConflictCondition reports the autoscaler conflicts of the summary.
func (r *TailoringReconciler) setConflictCondition(t *autoscalingv1alpha1.Tailoring) {
	s := t.Status.Scaling
	if s == nil || len(s.Autoscalers) == 0 {
		meta.RemoveStatusCondition(&t.Status.Conditions, ConditionTypeAutoscalerConflict)
		return
	}
	if len(s.Conflicts) == 0 {
		r.setCondition(t, ConditionTypeAutoscalerConflict, metav1.ConditionFalse, "NoConflict", "No two autoscalers act on the same resource")
		return
	}
	msgs := make([]string, 0, len(s.Conflicts))
	for _, c := range s.Conflicts {
		msgs = append(msgs, c.Message)
	}
	r.setCondition(t, ConditionTypeAutoscalerConflict, metav1.ConditionTrue, "AutoscalersConflict", strings.Join(msgs, "; "))
}

// alignedResources returns the resources sized for an HPA Utilization target.
func alignedResources(t *autoscalingv1alpha1.Tailoring) []string {
	if t.Status.Scaling == nil {
		return nil
	}
	return t.Status.Scaling.AlignedResources
}

// vpaWriteBackBounds returns the recommended VPA bounds raw write-back applies.
func vpaWriteBackBounds(t *autoscalingv1alpha1.Tailoring) map[string]map[string]writer.VPABounds {
	if t.Status.Scaling == nil {
		return nil
	}
	out := map[string]map[string]writer.VPABounds{}
	for _, v := range t.Status.Scaling.VPA {
		if !v.WriteBack {
			continue
		}
		containers := map[string]writer.VPABounds{}
		for _, c := range v.Containers {
			containers[c.Name] = writer.VPABounds{MinAllowed: writerValues(c.MinAllowed), MaxAllowed: writerValues(c.MaxAllowed)}
		}
		if len(containers) > 0 {
			out[v.Name] = containers
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func writerValues(v *autoscalingv1alpha1.ResourceValues) *writer.ResourceValues {
	if v == nil {
		return nil
	}
	return &writer.ResourceValues{CPU: v.CPU, Memory: v.Memory}
}
