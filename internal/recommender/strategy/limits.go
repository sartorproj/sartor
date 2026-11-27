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

import "k8s.io/apimachinery/pkg/api/resource"

// Metadata keys recording that a limit was raised to its request.
const (
	MetadataCPULimitRaisedToRequest    = "cpu_limit_raised_to_request"
	MetadataMemoryLimitRaisedToRequest = "memory_limit_raised_to_request"
)

// EnsureLimitsCoverRequests raises every CPU and memory limit that is below
// its request to the request, because Kubernetes rejects containers whose
// limits are lower than their requests. Requests, buffers, gradual change,
// safety rails and quotas are applied to each value separately, so this must
// run after all of them. Raised limits are recorded in rec.Metadata. A
// missing request or limit is left as is.
func EnsureLimitsCoverRequests(rec *Recommendation) {
	for _, r := range []struct {
		request *resource.Quantity
		limit   **resource.Quantity
		key     string
	}{
		{rec.Requests.CPU, &rec.Limits.CPU, MetadataCPULimitRaisedToRequest},
		{rec.Requests.Memory, &rec.Limits.Memory, MetadataMemoryLimitRaisedToRequest},
	} {
		if r.request == nil || *r.limit == nil || (*r.limit).Cmp(*r.request) >= 0 {
			continue
		}
		raised := r.request.DeepCopy()
		*r.limit = &raised
		if rec.Metadata == nil {
			rec.Metadata = make(map[string]interface{})
		}
		rec.Metadata[r.key] = true
	}
}
