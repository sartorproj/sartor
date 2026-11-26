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

package prometheus

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// Workload kinds whose pods can be matched.
const (
	KindDeployment  = "Deployment"
	KindStatefulSet = "StatefulSet"
	KindDaemonSet   = "DaemonSet"
)

// k8sSuffixChars is the alphabet Kubernetes uses for generated name suffixes
// and pod-template-hash values (k8s.io/apimachinery/pkg/util/rand).
const k8sSuffixChars = "[bcdfghjklmnpqrstvwxz2456789]"

// Workload identifies the workload whose pods are queried.
type Workload struct {
	// Kind is Deployment, StatefulSet or DaemonSet.
	Kind string
	// Name is the workload name.
	Name string
	// Cluster is matched against the client's ClusterLabel, if one is set.
	Cluster string
}

// PodNameRegex returns a fully anchored regular expression that matches the
// names of pods created by the workload's controller and nothing else that the
// naming scheme can tell apart:
//
//   - Deployment:  <name>-<pod-template-hash>-<5 chars>
//   - StatefulSet: <name>-<ordinal 0..9999>
//   - DaemonSet:   <name>-<5 chars>
//
// StatefulSet ordinals are capped at four digits so they can never collide
// with a five-character DaemonSet suffix made of digits.
//
// Names whose generated pod names exceed 63 characters are truncated by the
// API server and will not match; the kube-state-metrics owner join covers them.
func PodNameRegex(w Workload) (string, error) {
	if w.Name == "" {
		return "", fmt.Errorf("workload name is required")
	}
	name := regexp.QuoteMeta(w.Name)
	switch w.Kind {
	case KindDeployment:
		return fmt.Sprintf("^%s-%s{1,10}-%s{5}$", name, k8sSuffixChars, k8sSuffixChars), nil
	case KindStatefulSet:
		return fmt.Sprintf("^%s-(0|[1-9][0-9]{0,3})$", name), nil
	case KindDaemonSet:
		return fmt.Sprintf("^%s-%s{5}$", name, k8sSuffixChars), nil
	default:
		return "", fmt.Errorf("unsupported workload kind %q", w.Kind)
	}
}

// promString renders s as a PromQL/MetricsQL double-quoted string literal.
func promString(s string) string {
	return strconv.Quote(s)
}

// podSelector restricts cAdvisor container series to the pods of a workload.
type podSelector struct {
	namespace string
	workload  Workload
	podRegex  string
	// clusterLabel, when set, is matched against workload.Cluster on every series.
	clusterLabel string
	// ownerJoin selects pods through kube-state-metrics ownership series
	// instead of the pod-name regex.
	ownerJoin bool
}

// ValidateQueryTarget checks that namespace, workload name and (optional)
// container name are valid Kubernetes names, so they cannot carry PromQL.
func ValidateQueryTarget(namespace string, w Workload, containerName string) error {
	if errs := validation.IsDNS1123Label(namespace); len(errs) > 0 {
		return fmt.Errorf("invalid namespace %q: %s", namespace, strings.Join(errs, "; "))
	}
	if errs := validation.IsDNS1123Subdomain(w.Name); len(errs) > 0 {
		return fmt.Errorf("invalid workload name %q: %s", w.Name, strings.Join(errs, "; "))
	}
	if containerName != "" {
		if errs := validation.IsDNS1123Label(containerName); len(errs) > 0 {
			return fmt.Errorf("invalid container name %q: %s", containerName, strings.Join(errs, "; "))
		}
	}
	return nil
}

func newPodSelector(namespace string, w Workload, clusterLabel string) (podSelector, error) {
	if err := ValidateQueryTarget(namespace, w, ""); err != nil {
		return podSelector{}, err
	}
	if err := validateCluster(clusterLabel, w.Cluster); err != nil {
		return podSelector{}, err
	}
	re, err := PodNameRegex(w)
	if err != nil {
		return podSelector{}, err
	}
	return podSelector{namespace: namespace, workload: w, podRegex: re, clusterLabel: clusterLabel}, nil
}

var labelNameRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// validateCluster checks the cluster label name and requires a cluster value
// when a label is configured, so a query can never span clusters.
func validateCluster(clusterLabel, cluster string) error {
	if clusterLabel == "" {
		if cluster != "" {
			// Without the label the query would mix series of every cluster.
			return fmt.Errorf("target cluster %q is set but the Atelier sets no prometheus.clusterLabel", cluster)
		}
		return nil
	}
	if !labelNameRE.MatchString(clusterLabel) {
		return fmt.Errorf("invalid cluster label %q", clusterLabel)
	}
	if cluster == "" {
		return fmt.Errorf("a target cluster is required when the Atelier sets prometheus.clusterLabel (%s)", clusterLabel)
	}
	return nil
}

// scopeMatchers returns the cluster (if configured) and namespace matchers.
func (s podSelector) scopeMatchers() string {
	ns := "namespace=" + promString(s.namespace)
	if s.clusterLabel == "" {
		return ns
	}
	return s.clusterLabel + "=" + promString(s.workload.Cluster) + ", " + ns
}

// matchers returns the label matchers for a cAdvisor series, followed by extra.
func (s podSelector) matchers(extra ...string) string {
	podMatch := s.podRegex
	if s.ownerJoin {
		// The join is authoritative; the prefix only narrows the series scanned.
		podMatch = regexp.QuoteMeta(s.workload.Name) + "-.+"
	}
	parts := append([]string{
		s.scopeMatchers(),
		"pod=~" + promString(podMatch),
	}, extra...)
	return strings.Join(parts, ", ")
}

// scope wraps a per-pod expression so only the workload's pods remain.
func (s podSelector) scope(expr string) string {
	if !s.ownerJoin {
		return expr
	}
	return fmt.Sprintf("(%s) * on (namespace, pod) group_left() (%s)", expr, s.ownedPods())
}

// ownedPods returns an expression with one series (value 1) per pod owned by
// the workload, labelled with namespace and pod.
func (s podSelector) ownedPods() string {
	scope := s.scopeMatchers()
	name := promString(s.workload.Name)
	if s.workload.Kind == KindDeployment {
		return fmt.Sprintf(
			`max by (namespace, pod) (`+
				`label_replace(kube_pod_owner{%s, owner_kind="ReplicaSet"}, "replicaset", "$1", "owner_name", "(.+)")`+
				` * on (namespace, replicaset) group_left() `+
				`max by (namespace, replicaset) (kube_replicaset_owner{%s, owner_kind="Deployment", owner_name=%s})`+
				`)`,
			scope, scope, name)
	}
	return fmt.Sprintf(`max by (namespace, pod) (kube_pod_owner{%s, owner_kind=%s, owner_name=%s})`,
		scope, promString(s.workload.Kind), name)
}

// ownerProbeQuery returns an instant query that is non-empty when
// kube-state-metrics ownership series exist for the workload.
func (s podSelector) ownerProbeQuery() string {
	scope := s.scopeMatchers()
	name := promString(s.workload.Name)
	if s.workload.Kind == KindDeployment {
		return fmt.Sprintf(`count(kube_replicaset_owner{%s, owner_kind="Deployment", owner_name=%s})`, scope, name)
	}
	return fmt.Sprintf(`count(kube_pod_owner{%s, owner_kind=%s, owner_name=%s})`,
		scope, promString(s.workload.Kind), name)
}
