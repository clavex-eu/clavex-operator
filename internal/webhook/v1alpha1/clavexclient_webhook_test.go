/*
Copyright 2026.

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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	clavexv1alpha1 "github.com/clavex-eu/clavex-operator/api/v1alpha1"
	"github.com/clavex-eu/clavex-operator/internal/authsecret"
)

const (
	testWebhookNamespace     = "default"
	testMissingAuthSecretRef = "does-not-exist-secret"
)

var _ = Describe("ClavexClient Webhook", func() {
	var (
		obj       *clavexv1alpha1.ClavexClient
		oldObj    *clavexv1alpha1.ClavexClient
		validator ClavexClientCustomValidator
	)

	BeforeEach(func() {
		validator = ClavexClientCustomValidator{Client: k8sClient}

		spec := clavexv1alpha1.ClavexClientSpec{
			OrgRef:       "acme",
			ClientID:     "acme-webhook-test-client",
			Name:         "Acme Webhook Test Client",
			RedirectURIs: []string{"https://acme.example/callback"},
			GrantTypes:   []string{"authorization_code"},
		}
		obj = &clavexv1alpha1.ClavexClient{Spec: spec}
		oldObj = &clavexv1alpha1.ClavexClient{Spec: spec}
		obj.Namespace = testWebhookNamespace
		oldObj.Namespace = testWebhookNamespace

		Expect(validator).NotTo(BeNil(), "Expected validator to be initialized")
		Expect(oldObj).NotTo(BeNil(), "Expected oldObj to be initialized")
		Expect(obj).NotTo(BeNil(), "Expected obj to be initialized")
	})

	Context("When creating or updating ClavexClient under Validating Webhook", func() {
		It("Should deny creation when authSecretRef does not resolve to an existing Secret", func() {
			obj.Spec.AuthSecretRef = clavexv1alpha1.SecretRef{Name: testMissingAuthSecretRef}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.authSecretRef is invalid"))
		})

		It("Should deny creation when the referenced Secret is missing the orgId key", func() {
			secretName := "incomplete-auth-secret"
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: testWebhookNamespace},
				Data: map[string][]byte{
					authsecret.DefaultAPIKeySecretKey: []byte("test-api-key"),
					// orgId intentionally omitted.
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, secret) })

			obj.Spec.AuthSecretRef = clavexv1alpha1.SecretRef{Name: secretName}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("missing key"))
		})

		It("Should admit creation when the referenced Secret exists with the required keys", func() {
			secretName := "complete-auth-secret"
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: testWebhookNamespace},
				Data: map[string][]byte{
					authsecret.DefaultAPIKeySecretKey: []byte("test-api-key"),
					authsecret.OrgIDSecretKey:         []byte("11111111-1111-1111-1111-111111111111"),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, secret) })

			obj.Spec.AuthSecretRef = clavexv1alpha1.SecretRef{Name: secretName}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should apply the same authSecretRef check on update", func() {
			obj.Spec.AuthSecretRef = clavexv1alpha1.SecretRef{Name: testMissingAuthSecretRef}
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.authSecretRef is invalid"))
		})

		It("Should always admit deletion regardless of authSecretRef state", func() {
			obj.Spec.AuthSecretRef = clavexv1alpha1.SecretRef{Name: testMissingAuthSecretRef}
			_, err := validator.ValidateDelete(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
