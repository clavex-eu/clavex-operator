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

package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	clavexv1alpha1 "github.com/clavex-eu/clavex-operator/api/v1alpha1"
	"github.com/clavex-eu/clavex-operator/internal/authsecret"
)

const (
	testWebhookURL     = "https://example.test/hooks/clavex"
	testWebhookAPIKey  = "clv_test-org-scoped-webhook-key"
	testWebhookEventA  = "user.login"
	testWebhookEventB  = "user.password.changed"
	testWebhookSecretV = "hmac-s3cr3t"
)

// fakeWebhookAdminAPI is a minimal httptest-backed stand-in for the Clavex
// Admin API's webhook endpoints. Unlike ClavexClient, there is no
// client-supplied ID and — unlike ClavexIdentityProvider — no name field
// either, so the fake models the endpoints exactly as the real API does:
// List/Create/Update/Delete keyed by an API-assigned ID, matched here by
// URL (see ClavexWebhookSpec's doc comment).
type fakeWebhookAdminAPI struct {
	*httptest.Server
	webhookID     string
	liveEvents    []string
	liveIsActive  bool
	created       bool
	patchCalled   bool
	lastCreateKey string
	lastUpdateKey string
}

func newFakeWebhookAdminAPI(orgID string) *fakeWebhookAdminAPI {
	f := &fakeWebhookAdminAPI{
		webhookID:    "66666666-6666-6666-6666-666666666666",
		liveEvents:   []string{testWebhookEventA},
		liveIsActive: true,
	}
	mux := http.NewServeMux()
	base := "/api/v1/organizations/" + orgID + "/webhooks"

	webhookPayload := func() map[string]any {
		return map[string]any{
			jsonFieldID:       f.webhookID,
			jsonFieldOrgID:    orgID,
			"url":             testWebhookURL,
			"events":          f.liveEvents,
			jsonFieldIsActive: f.liveIsActive,
		}
	}

	mux.HandleFunc(base, func(w http.ResponseWriter, r *http.Request) {
		Expect(r.Header.Get("X-API-Key")).To(Equal(testWebhookAPIKey))
		switch r.Method {
		case http.MethodGet:
			if !f.created {
				_ = json.NewEncoder(w).Encode([]map[string]any{})
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{webhookPayload()})
		case http.MethodPost:
			var body map[string]any
			Expect(json.NewDecoder(r.Body).Decode(&body)).To(Succeed())
			if secret, ok := body["secret"].(string); ok {
				f.lastCreateKey = secret
			}
			f.created = true
			f.liveEvents = []string{testWebhookEventA}
			f.liveIsActive = true
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(webhookPayload())
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc(base+"/"+f.webhookID, func(w http.ResponseWriter, r *http.Request) {
		Expect(r.Header.Get("X-API-Key")).To(Equal(testWebhookAPIKey))
		switch r.Method {
		case http.MethodPatch:
			f.patchCalled = true
			var body map[string]any
			Expect(json.NewDecoder(r.Body).Decode(&body)).To(Succeed())
			if secret, ok := body["secret"].(string); ok {
				f.lastUpdateKey = secret
			}
			f.liveEvents = []string{testWebhookEventA} // PATCH resyncs to spec
			f.liveIsActive = true
			_ = json.NewEncoder(w).Encode(webhookPayload())
		case http.MethodDelete:
			f.created = false
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	f.Server = httptest.NewServer(mux)
	return f
}

var _ = Describe("ClavexWebhook Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-webhook-resource"
			resourceNamespace = "default"
			authSecretName    = "test-webhook-auth-secret"
			signingKeyName    = "test-webhook-signing-key"
			orgID             = "11111111-1111-1111-1111-111111111111"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}
		clavexwebhook := &clavexv1alpha1.ClavexWebhook{}
		var fakeAPI *fakeWebhookAdminAPI
		var controllerReconciler *ClavexWebhookReconciler

		BeforeEach(func() {
			fakeAPI = newFakeWebhookAdminAPI(orgID)
			controllerReconciler = &ClavexWebhookReconciler{
				Client:          k8sClient,
				Scheme:          k8sClient.Scheme(),
				ClavexServerURL: fakeAPI.URL,
			}

			By("creating the auth Secret referenced by authSecretRef")
			authSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      authSecretName,
					Namespace: resourceNamespace,
				},
				StringData: map[string]string{
					authsecret.DefaultAPIKeySecretKey: testWebhookAPIKey,
					authsecret.OrgIDSecretKey:         orgID,
				},
			}
			Expect(k8sClient.Create(ctx, authSecret)).To(Succeed())

			By("creating the signing key Secret referenced by signingKeyRef")
			signingKeySecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      signingKeyName,
					Namespace: resourceNamespace,
				},
				StringData: map[string]string{
					"clientSecret": testWebhookSecretV,
				},
			}
			Expect(k8sClient.Create(ctx, signingKeySecret)).To(Succeed())

			By("creating the custom resource for the Kind ClavexWebhook")
			err := k8sClient.Get(ctx, typeNamespacedName, clavexwebhook)
			if err != nil && errors.IsNotFound(err) {
				resource := &clavexv1alpha1.ClavexWebhook{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: resourceNamespace,
					},
					Spec: clavexv1alpha1.ClavexWebhookSpec{
						OrgRef: testOrgRef,
						AuthSecretRef: clavexv1alpha1.SecretRef{
							Name: authSecretName,
						},
						URL:     testWebhookURL,
						Events:  []string{testWebhookEventA},
						Enabled: true,
						SigningKeyRef: &clavexv1alpha1.SecretRef{
							Name: signingKeyName,
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			By("Cleanup the specific resource instance ClavexWebhook")
			resource := &clavexv1alpha1.ClavexWebhook{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(HaveOccurred())

			authSecret := &corev1.Secret{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: authSecretName, Namespace: resourceNamespace}, authSecret); err == nil {
				Expect(k8sClient.Delete(ctx, authSecret)).To(Succeed())
			}
			signingKeySecret := &corev1.Secret{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: signingKeyName, Namespace: resourceNamespace}, signingKeySecret); err == nil {
				Expect(k8sClient.Delete(ctx, signingKeySecret)).To(Succeed())
			}

			fakeAPI.Close()
		})

		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the status reflects a successful sync")
			resource := &clavexv1alpha1.ClavexWebhook{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			Expect(resource.Status.ClavexWebhookID).To(Equal(fakeAPI.webhookID))

			var readyCond *metav1.Condition
			for i := range resource.Status.Conditions {
				if resource.Status.Conditions[i].Type == conditionTypeReady {
					readyCond = &resource.Status.Conditions[i]
				}
			}
			Expect(readyCond).ToNot(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))

			By("verifying the signing key was resolved from the Secret and sent to the Admin API")
			Expect(fakeAPI.lastCreateKey).To(Equal(testWebhookSecretV))
		})

		It("should detect and correct out-of-band drift without a spec change", func() {
			By("reconciling once to create the remote webhook")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("simulating an out-of-band edit directly against the Admin API")
			fakeAPI.liveEvents = []string{testWebhookEventA, testWebhookEventB}
			fakeAPI.patchCalled = false

			By("reconciling again with no spec change")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the drift was corrected via PATCH")
			Expect(fakeAPI.patchCalled).To(BeTrue())
			Expect(fakeAPI.liveEvents).To(Equal([]string{testWebhookEventA}))

			By("verifying the Synced condition reports DriftCorrected")
			resource := &clavexv1alpha1.ClavexWebhook{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			var syncedCond *metav1.Condition
			for i := range resource.Status.Conditions {
				if resource.Status.Conditions[i].Type == conditionTypeSynced {
					syncedCond = &resource.Status.Conditions[i]
				}
			}
			Expect(syncedCond).ToNot(BeNil())
			Expect(syncedCond.Reason).To(Equal("DriftCorrected"))
		})
	})
})
