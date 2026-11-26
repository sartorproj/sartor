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

package oom

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFindTailoring_TargetNamespace: a Tailoring whose target lives in another
// namespace does not own a same-named workload in its own namespace.
func TestFindTailoring_TargetNamespace(t *testing.T) {
	elsewhere := tailoringFor("Deployment", "web", false)
	elsewhere.Name, elsewhere.Spec.Target.Namespace = "elsewhere", "other"
	explicit := tailoringFor("Deployment", "web", false)
	explicit.Name, explicit.Spec.Target.Namespace = "explicit", explicit.Namespace

	event := OOMEvent{Namespace: explicit.Namespace, OwnerKind: "Deployment", OwnerName: "web"}

	i := NewInterceptor(newFakeClient(elsewhere), InterceptorConfig{Enabled: true})
	found, err := i.findTailoring(context.Background(), event)
	require.NoError(t, err)
	assert.Nil(t, found)

	i = NewInterceptor(newFakeClient(elsewhere, explicit), InterceptorConfig{Enabled: true})
	found, err = i.findTailoring(context.Background(), event)
	require.NoError(t, err)
	require.NotNil(t, found)
	assert.Equal(t, "explicit", found.Name)
}
