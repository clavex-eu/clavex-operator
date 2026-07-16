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
	testAuthPolicyName   = "block-high-risk-countries"
	testAuthPolicyAPIKey = "clv_test-org-scoped-authpolicy-key"
	testAuthPolicyAction = "deny"
)

// fakeAuthPolicyAdminAPI is a minimal httptest-backed stand-in for the
// Clavex Admin API's auth-policies endpoints. Like ClavexIdentityProvider,
// there is no client-supplied ID, so the fake models List/Create/Update/
// Delete keyed by an API-assigned ID, matched by Name (see
// ClavexAuthPolicySpec's doc comment).
type fakeAuthPolicyAdminAPI struct {
	*httptest.Server
	ruleID         string
	livePriority   int
	liveAction     string
	liveEnabled    bool
	liveCountries  []string
	created        bool
	putCalled      bool
	lastCreateBody map[string]any
	lastUpdateBody map[string]any
}

func newFakeAuthPolicyAdminAPI(orgID string) *fakeAuthPolicyAdminAPI {
	f := &fakeAuthPolicyAdminAPI{
		ruleID:        "77777777-7777-7777-7777-777777777777",
		livePriority:  10,
		liveAction:    testAuthPolicyAction,
		liveEnabled:   true,
		liveCountries: []string{"RU"},
	}
	mux := http.NewServeMux()
	base := "/api/v1/organizations/" + orgID + "/auth-policies"

	rulePayload := func() map[string]any {
		return map[string]any{
			jsonFieldID:    f.ruleID,
			jsonFieldOrgID: orgID,
			jsonFieldName:  testAuthPolicyName,
			"priority":     f.livePriority,
			"action":       f.liveAction,
			"enabled":      f.liveEnabled,
			"conditions": map[string]any{
				"country": f.liveCountries,
			},
		}
	}

	mux.HandleFunc(base, func(w http.ResponseWriter, r *http.Request) {
		Expect(r.Header.Get("X-API-Key")).To(Equal(testAuthPolicyAPIKey))
		switch r.Method {
		case http.MethodGet:
			if !f.created {
				_ = json.NewEncoder(w).Encode([]map[string]any{})
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{rulePayload()})
		case http.MethodPost:
			var body map[string]any
			Expect(json.NewDecoder(r.Body).Decode(&body)).To(Succeed())
			f.lastCreateBody = body
			f.created = true
			f.livePriority = 10
			f.liveAction = testAuthPolicyAction
			f.liveEnabled = true
			f.liveCountries = []string{"RU"}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(rulePayload())
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc(base+"/"+f.ruleID, func(w http.ResponseWriter, r *http.Request) {
		Expect(r.Header.Get("X-API-Key")).To(Equal(testAuthPolicyAPIKey))
		switch r.Method {
		case http.MethodPut:
			f.putCalled = true
			var body map[string]any
			Expect(json.NewDecoder(r.Body).Decode(&body)).To(Succeed())
			f.lastUpdateBody = body
			f.livePriority = 10 // PUT resyncs to spec
			f.liveAction = testAuthPolicyAction
			f.liveEnabled = true
			f.liveCountries = []string{"RU"}
			_ = json.NewEncoder(w).Encode(rulePayload())
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

var _ = Describe("ClavexAuthPolicy Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-authpolicy-resource"
			resourceNamespace = "default"
			authSecretName    = "test-authpolicy-auth-secret"
			orgID             = "33333333-3333-3333-3333-333333333333"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}
		clavexauthpolicy := &clavexv1alpha1.ClavexAuthPolicy{}
		var fakeAPI *fakeAuthPolicyAdminAPI
		var controllerReconciler *ClavexAuthPolicyReconciler

		BeforeEach(func() {
			fakeAPI = newFakeAuthPolicyAdminAPI(orgID)
			controllerReconciler = &ClavexAuthPolicyReconciler{
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
					authsecret.DefaultAPIKeySecretKey: testAuthPolicyAPIKey,
					authsecret.OrgIDSecretKey:         orgID,
				},
			}
			Expect(k8sClient.Create(ctx, authSecret)).To(Succeed())

			By("creating the custom resource for the Kind ClavexAuthPolicy")
			err := k8sClient.Get(ctx, typeNamespacedName, clavexauthpolicy)
			if err != nil && errors.IsNotFound(err) {
				resource := &clavexv1alpha1.ClavexAuthPolicy{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: resourceNamespace,
					},
					Spec: clavexv1alpha1.ClavexAuthPolicySpec{
						OrgRef: testOrgRef,
						AuthSecretRef: clavexv1alpha1.SecretRef{
							Name: authSecretName,
						},
						Name:     testAuthPolicyName,
						Priority: 10,
						Action:   testAuthPolicyAction,
						Enabled:  true,
						Conditions: clavexv1alpha1.AuthPolicyConditions{
							Countries: []string{"RU"},
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			By("Cleanup the specific resource instance ClavexAuthPolicy")
			resource := &clavexv1alpha1.ClavexAuthPolicy{}
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

			fakeAPI.Close()
		})

		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the status reflects a successful sync")
			resource := &clavexv1alpha1.ClavexAuthPolicy{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			Expect(resource.Status.ClavexAuthPolicyID).To(Equal(fakeAPI.ruleID))

			var readyCond *metav1.Condition
			for i := range resource.Status.Conditions {
				if resource.Status.Conditions[i].Type == conditionTypeReady {
					readyCond = &resource.Status.Conditions[i]
				}
			}
			Expect(readyCond).ToNot(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))

			By("verifying the rule was created with the right action/conditions")
			Expect(fakeAPI.lastCreateBody["action"]).To(Equal(testAuthPolicyAction))
		})

		It("should detect and correct out-of-band drift without a spec change", func() {
			By("reconciling once to create the remote policy rule")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("simulating an out-of-band edit directly against the Admin API")
			fakeAPI.liveAction = "allow"
			fakeAPI.putCalled = false

			By("reconciling again with no spec change")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the drift was corrected via PUT")
			Expect(fakeAPI.putCalled).To(BeTrue())
			Expect(fakeAPI.liveAction).To(Equal(testAuthPolicyAction))

			By("verifying the Synced condition reports DriftCorrected")
			resource := &clavexv1alpha1.ClavexAuthPolicy{}
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
