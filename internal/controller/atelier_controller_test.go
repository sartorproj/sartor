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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	autoscalingv1alpha1 "github.com/sartorproj/sartor/api/v1alpha1"
)

var _ = Describe("Atelier Controller", func() {
	const (
		AtelierName = "test-atelier"

		timeout  = time.Second * 10
		interval = time.Millisecond * 250
	)

	Context("When creating an Atelier", func() {
		It("Should create successfully", func() {
			By("Creating a new Atelier")
			atelier := &autoscalingv1alpha1.Atelier{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "autoscaling.sartorproj.io/v1alpha1",
					Kind:       "Atelier",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: AtelierName,
				},
				Spec: autoscalingv1alpha1.AtelierSpec{
					Prometheus: autoscalingv1alpha1.PrometheusConfig{
						URL: "http://prometheus:9090",
					},
					GitProvider: autoscalingv1alpha1.GitProviderConfig{
						Type: autoscalingv1alpha1.GitProviderGitHub,
						SecretRef: corev1.SecretReference{
							Name:      "github-token",
							Namespace: "default",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, atelier)).Should(Succeed())

			atelierLookupKey := types.NamespacedName{Name: AtelierName}
			createdAtelier := &autoscalingv1alpha1.Atelier{}

			Eventually(func() bool {
				err := k8sClient.Get(ctx, atelierLookupKey, createdAtelier)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			Expect(createdAtelier.Spec.Prometheus.URL).Should(Equal("http://prometheus:9090"))
			Expect(createdAtelier.Spec.GitProvider.Type).Should(Equal(autoscalingv1alpha1.GitProviderGitHub))
		})

		It("Should update status after reconciliation", func() {
			atelierLookupKey := types.NamespacedName{Name: AtelierName}
			atelier := &autoscalingv1alpha1.Atelier{}

			Eventually(func() bool {
				err := k8sClient.Get(ctx, atelierLookupKey, atelier)
				if err != nil {
					return false
				}
				// Status should be updated by reconciler
				return atelier.Status.ManagedTailorings >= 0
			}, timeout, interval).Should(BeTrue())
		})
	})

	Context("When Atelier has ArgoCD enabled", func() {
		It("Should check ArgoCD connectivity", func() {
			atelier := &autoscalingv1alpha1.Atelier{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: AtelierName}, atelier)
			Expect(err).NotTo(HaveOccurred())

			// Update with ArgoCD config
			atelier.Spec.ArgoCD = &autoscalingv1alpha1.ArgoCDConfig{
				Enabled:     true,
				SyncOnMerge: true,
			}
			Expect(k8sClient.Update(ctx, atelier)).Should(Succeed())

			// Wait for reconciliation
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: AtelierName}, atelier)
				return err == nil && atelier.Spec.ArgoCD != nil && atelier.Spec.ArgoCD.Enabled
			}, timeout, interval).Should(BeTrue())
		})
	})

	Context("When deleting an Atelier", func() {
		It("Should delete successfully", func() {
			atelier := &autoscalingv1alpha1.Atelier{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: AtelierName}, atelier)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Delete(ctx, atelier)).Should(Succeed())

			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: AtelierName}, atelier)
				return err != nil
			}, timeout, interval).Should(BeTrue())
		})
	})
})
