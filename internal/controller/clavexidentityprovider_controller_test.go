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
	testIDPName         = "Test OIDC Provider"
	testIDPProviderType = "oidc"
	testIDPClientID     = "test-idp-client"
	testIDPAPIKeyHeader = "clv_test-org-scoped-idp-key"
)

// fakeIDPAdminAPI is a minimal httptest-backed stand-in for the Clavex
// Admin API's identity-provider endpoints. Unlike ClavexClient, there is no
// client-supplied ID, so the reconciler always looks the resource up via
// List — the fake therefore needs to support List/Create/Update/Delete
// rather than a Get-by-ID.
type fakeIDPAdminAPI struct {
	*httptest.Server
	idpID string
	// liveAllowJIT lets a test simulate an out-of-band edit (e.g. via
	// clavexctl or the Admin UI) by making List return a value that no
	// longer matches the CR's spec. Unlike ClavexClient (looked up by a
	// stable client-supplied ID), ClavexIdentityProvider is looked up by
	// Name — drifting the Name itself would make findIDPByName fail to
	// locate the record at all, so tests must drift a non-key field.
	liveAllowJIT bool
	created      bool
	patchCalled  bool
	lastCreateCS string // last client_secret seen on POST, for assertions
	lastUpdateCS string // last client_secret seen on PATCH
}

func newFakeIDPAdminAPI(orgID string) *fakeIDPAdminAPI {
	f := &fakeIDPAdminAPI{idpID: "22222222-2222-2222-2222-222222222222"}
	mux := http.NewServeMux()
	base := "/api/v1/organizations/" + orgID + "/identity-providers"

	idpPayload := func() map[string]any {
		return map[string]any{
			"id":              f.idpID,
			jsonFieldOrgID:    orgID,
			jsonFieldName:     testIDPName,
			"provider_type":   testIDPProviderType,
			"client_id":       testIDPClientID,
			jsonFieldIsActive: true,
			"allow_jit":       f.liveAllowJIT,
		}
	}

	mux.HandleFunc(base, func(w http.ResponseWriter, r *http.Request) {
		Expect(r.Header.Get("X-API-Key")).To(Equal(testIDPAPIKeyHeader))
		switch r.Method {
		case http.MethodGet:
			if !f.created {
				_ = json.NewEncoder(w).Encode([]map[string]any{})
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{idpPayload()})
		case http.MethodPost:
			var body map[string]any
			Expect(json.NewDecoder(r.Body).Decode(&body)).To(Succeed())
			if cfg, ok := body["config"].(map[string]any); ok {
				if cs, ok := cfg["client_secret"].(string); ok {
					f.lastCreateCS = cs
				}
			}
			f.created = true
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(idpPayload())
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc(base+"/"+f.idpID, func(w http.ResponseWriter, r *http.Request) {
		Expect(r.Header.Get("X-API-Key")).To(Equal(testIDPAPIKeyHeader))
		switch r.Method {
		case http.MethodPatch:
			f.patchCalled = true
			var body map[string]any
			Expect(json.NewDecoder(r.Body).Decode(&body)).To(Succeed())
			if cfg, ok := body["config"].(map[string]any); ok {
				if cs, ok := cfg["client_secret"].(string); ok {
					f.lastUpdateCS = cs
				}
			}
			f.liveAllowJIT = false // PATCH resyncs the live IdP to spec
			_ = json.NewEncoder(w).Encode(idpPayload())
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

var _ = Describe("ClavexIdentityProvider Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName        = "test-idp-resource"
			resourceNamespace   = "default"
			authSecretName      = "test-idp-auth-secret"
			clientSecretName    = "test-idp-client-secret"
			orgID               = "11111111-1111-1111-1111-111111111111"
			plainClientSecretVl = "s3cr3t-from-user"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}
		clavexidp := &clavexv1alpha1.ClavexIdentityProvider{}
		var fakeAPI *fakeIDPAdminAPI
		var controllerReconciler *ClavexIdentityProviderReconciler

		BeforeEach(func() {
			fakeAPI = newFakeIDPAdminAPI(orgID)
			controllerReconciler = &ClavexIdentityProviderReconciler{
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
					authsecret.DefaultAPIKeySecretKey: testIDPAPIKeyHeader,
					authsecret.OrgIDSecretKey:         orgID,
				},
			}
			Expect(k8sClient.Create(ctx, authSecret)).To(Succeed())

			By("creating the client Secret referenced by clientSecretRef")
			clientSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clientSecretName,
					Namespace: resourceNamespace,
				},
				StringData: map[string]string{
					"clientSecret": plainClientSecretVl,
				},
			}
			Expect(k8sClient.Create(ctx, clientSecret)).To(Succeed())

			By("creating the custom resource for the Kind ClavexIdentityProvider")
			err := k8sClient.Get(ctx, typeNamespacedName, clavexidp)
			if err != nil && errors.IsNotFound(err) {
				resource := &clavexv1alpha1.ClavexIdentityProvider{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: resourceNamespace,
					},
					Spec: clavexv1alpha1.ClavexIdentityProviderSpec{
						OrgRef: testOrgRef,
						AuthSecretRef: clavexv1alpha1.SecretRef{
							Name: authSecretName,
						},
						Name:         testIDPName,
						ProviderType: testIDPProviderType,
						Enabled:      true,
						ClientID:     testIDPClientID,
						ClientSecretRef: &clavexv1alpha1.SecretRef{
							Name: clientSecretName,
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			By("Cleanup the specific resource instance ClavexIdentityProvider")
			resource := &clavexv1alpha1.ClavexIdentityProvider{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			// No manager/controller is running in the background during
			// these tests, so the finalizer added by Reconcile is never
			// removed automatically. Drive one more reconcile pass here to
			// let the reconciler process the deletion (remove the remote
			// IdP + finalizer) — otherwise the CR would leak into the next
			// spec with a stale DeletionTimestamp still set.
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(HaveOccurred())

			authSecret := &corev1.Secret{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: authSecretName, Namespace: resourceNamespace}, authSecret); err == nil {
				Expect(k8sClient.Delete(ctx, authSecret)).To(Succeed())
			}
			clientSecret := &corev1.Secret{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: clientSecretName, Namespace: resourceNamespace}, clientSecret); err == nil {
				Expect(k8sClient.Delete(ctx, clientSecret)).To(Succeed())
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
			resource := &clavexv1alpha1.ClavexIdentityProvider{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			Expect(resource.Status.ClavexIDPID).To(Equal(fakeAPI.idpID))
			Expect(resource.Status.Conditions).ToNot(BeEmpty())

			var readyCond *metav1.Condition
			for i := range resource.Status.Conditions {
				if resource.Status.Conditions[i].Type == conditionTypeReady {
					readyCond = &resource.Status.Conditions[i]
				}
			}
			Expect(readyCond).ToNot(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))

			By("verifying the client secret was resolved from the Secret and sent to the Admin API")
			Expect(fakeAPI.lastCreateCS).To(Equal(plainClientSecretVl))
		})

		It("should detect and correct out-of-band drift without a spec change", func() {
			By("reconciling once to create the remote identity provider")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("simulating an out-of-band edit directly against the Admin API")
			fakeAPI.liveAllowJIT = true
			fakeAPI.patchCalled = false

			By("reconciling again with no spec change")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the drift was corrected via PATCH")
			Expect(fakeAPI.patchCalled).To(BeTrue())
			Expect(fakeAPI.liveAllowJIT).To(BeFalse())

			By("verifying the Synced condition reports DriftCorrected")
			resource := &clavexv1alpha1.ClavexIdentityProvider{}
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
