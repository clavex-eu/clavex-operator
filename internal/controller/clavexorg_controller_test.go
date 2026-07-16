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

const testOrgAPIKey = "clv_test-org-scoped-settings-key"

// fakeOrgAdminAPI is a minimal httptest-backed stand-in for the Clavex
// Admin API's org-settings endpoints (password-policy, rate-limits).
// Unlike the other CRDs' fakes, these resources have no create/delete
// semantics — GET always returns the live value (seeded with defaults
// that differ from the desired spec, to exercise drift correction on the
// very first reconcile) and PUT always succeeds, mirroring
// PasswordPolicyService/RateLimitService's Get/Put-only shape.
type fakeOrgAdminAPI struct {
	*httptest.Server
	livePasswordPolicy map[string]any
	liveRateLimits     map[string]any
	passwordPutCalled  bool
	rateLimitPutCalled bool
}

func newFakeOrgAdminAPI(orgID string) *fakeOrgAdminAPI {
	f := &fakeOrgAdminAPI{
		livePasswordPolicy: map[string]any{
			jsonFieldOrgID:    orgID,
			"min_length":      8,
			"require_upper":   false,
			"require_lower":   true,
			"require_number":  false,
			"require_special": false,
			"max_age_days":    0,
			"history_count":   0,
		},
		liveRateLimits: map[string]any{
			jsonFieldOrgID:               orgID,
			"max_attempts_per_minute":    60,
			"lockout_duration_seconds":   0,
			"ip_max_attempts_per_minute": 0,
		},
	}
	mux := http.NewServeMux()
	base := "/api/v1/organizations/" + orgID

	mux.HandleFunc(base+"/password-policy", func(w http.ResponseWriter, r *http.Request) {
		Expect(r.Header.Get("X-API-Key")).To(Equal(testOrgAPIKey))
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(f.livePasswordPolicy)
		case http.MethodPut:
			f.passwordPutCalled = true
			var body map[string]any
			Expect(json.NewDecoder(r.Body).Decode(&body)).To(Succeed())
			f.livePasswordPolicy = body
			_ = json.NewEncoder(w).Encode(f.livePasswordPolicy)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc(base+"/rate-limits", func(w http.ResponseWriter, r *http.Request) {
		Expect(r.Header.Get("X-API-Key")).To(Equal(testOrgAPIKey))
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(f.liveRateLimits)
		case http.MethodPut:
			f.rateLimitPutCalled = true
			var body map[string]any
			Expect(json.NewDecoder(r.Body).Decode(&body)).To(Succeed())
			f.liveRateLimits = body
			_ = json.NewEncoder(w).Encode(f.liveRateLimits)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	f.Server = httptest.NewServer(mux)
	return f
}

var _ = Describe("ClavexOrg Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-org-resource"
			resourceNamespace = "default"
			authSecretName    = "test-org-auth-secret"
			orgID             = "22222222-2222-2222-2222-222222222222"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}
		clavexorg := &clavexv1alpha1.ClavexOrg{}
		var fakeAPI *fakeOrgAdminAPI
		var controllerReconciler *ClavexOrgReconciler

		BeforeEach(func() {
			fakeAPI = newFakeOrgAdminAPI(orgID)
			controllerReconciler = &ClavexOrgReconciler{
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
					authsecret.DefaultAPIKeySecretKey: testOrgAPIKey,
					authsecret.OrgIDSecretKey:         orgID,
				},
			}
			Expect(k8sClient.Create(ctx, authSecret)).To(Succeed())

			By("creating the custom resource for the Kind ClavexOrg")
			err := k8sClient.Get(ctx, typeNamespacedName, clavexorg)
			if err != nil && errors.IsNotFound(err) {
				resource := &clavexv1alpha1.ClavexOrg{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: resourceNamespace,
					},
					Spec: clavexv1alpha1.ClavexOrgSpec{
						OrgRef: testOrgRef,
						AuthSecretRef: clavexv1alpha1.SecretRef{
							Name: authSecretName,
						},
						PasswordPolicy: &clavexv1alpha1.PasswordPolicySpec{
							MinLength:      12,
							RequireUpper:   true,
							RequireLower:   true,
							RequireNumber:  true,
							RequireSpecial: true,
							MaxAgeDays:     90,
							HistoryCount:   5,
						},
						RateLimits: &clavexv1alpha1.RateLimitsSpec{
							MaxAttemptsPerMinute:   5,
							LockoutDurationSeconds: 300,
							IPMaxAttemptsPerMinute: 20,
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			By("Cleanup the specific resource instance ClavexOrg")
			resource := &clavexv1alpha1.ClavexOrg{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			authSecret := &corev1.Secret{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: authSecretName, Namespace: resourceNamespace}, authSecret); err == nil {
				Expect(k8sClient.Delete(ctx, authSecret)).To(Succeed())
			}

			fakeAPI.Close()
		})

		It("should successfully reconcile the resource, converging password policy and rate limits", func() {
			By("Reconciling the created resource")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying both settings sections were PUT to the Admin API")
			Expect(fakeAPI.passwordPutCalled).To(BeTrue())
			Expect(fakeAPI.rateLimitPutCalled).To(BeTrue())
			Expect(fakeAPI.livePasswordPolicy["min_length"]).To(BeNumerically("==", 12))
			Expect(fakeAPI.liveRateLimits["max_attempts_per_minute"]).To(BeNumerically("==", 5))

			By("verifying the status reflects a successful sync")
			resource := &clavexv1alpha1.ClavexOrg{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())

			var readyCond *metav1.Condition
			for i := range resource.Status.Conditions {
				if resource.Status.Conditions[i].Type == conditionTypeReady {
					readyCond = &resource.Status.Conditions[i]
				}
			}
			Expect(readyCond).ToNot(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
		})

		It("should detect and correct out-of-band drift without a spec change", func() {
			By("reconciling once to converge settings")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("simulating an out-of-band edit directly against the Admin API")
			fakeAPI.liveRateLimits["max_attempts_per_minute"] = 100
			fakeAPI.rateLimitPutCalled = false

			By("reconciling again with no spec change")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the drift was corrected via PUT")
			Expect(fakeAPI.rateLimitPutCalled).To(BeTrue())
			Expect(fakeAPI.liveRateLimits["max_attempts_per_minute"]).To(BeNumerically("==", 5))

			By("verifying the Synced condition reports DriftCorrected")
			resource := &clavexv1alpha1.ClavexOrg{}
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
