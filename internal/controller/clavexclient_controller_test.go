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
	testClientName        = "Test Client"
	testClientRedirectURI = "https://example.test/callback"
	testClientGrantType   = "authorization_code"
	testAPIKeyHeaderValue = "clv_test-org-scoped-key"
)

// fakeAdminAPI is a minimal httptest-backed stand-in for the Clavex Admin
// API, just enough to drive the ClavexClient reconciler through create,
// drift-correction and delete without a real Clavex server.
type fakeAdminAPI struct {
	*httptest.Server
	created bool
	// liveName lets a test simulate an out-of-band edit (e.g. via
	// clavexctl or the Admin UI) by making the next GET return a name
	// that no longer matches the CR's spec.
	liveName    string
	patchCalled bool
}

func newFakeAdminAPI(orgID, clientID string) *fakeAdminAPI {
	f := &fakeAdminAPI{liveName: testClientName}
	mux := http.NewServeMux()
	base := "/api/v1/organizations/" + orgID + "/clients"

	clientPayload := func() map[string]any {
		return map[string]any{
			"client_id":       clientID,
			jsonFieldOrgID:    orgID,
			jsonFieldName:     f.liveName,
			"redirect_uris":   []string{testClientRedirectURI},
			"grant_types":     []string{testClientGrantType},
			jsonFieldIsActive: true,
		}
	}

	mux.HandleFunc(base+"/"+clientID, func(w http.ResponseWriter, r *http.Request) {
		Expect(r.Header.Get("X-API-Key")).To(Equal(testAPIKeyHeaderValue))
		switch r.Method {
		case http.MethodGet:
			if !f.created {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"not found"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(clientPayload())
		case http.MethodPatch:
			f.patchCalled = true
			f.liveName = testClientName // PATCH resyncs the live client to spec
			_ = json.NewEncoder(w).Encode(clientPayload())
		case http.MethodDelete:
			f.created = false
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc(base, func(w http.ResponseWriter, r *http.Request) {
		Expect(r.Method).To(Equal(http.MethodPost))
		Expect(r.Header.Get("X-API-Key")).To(Equal(testAPIKeyHeaderValue))
		f.created = true
		f.liveName = testClientName
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"client":        clientPayload(),
			"client_secret": "s3cr3t",
		})
	})

	f.Server = httptest.NewServer(mux)
	return f
}

var _ = Describe("ClavexClient Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-resource"
			resourceNamespace = "default"
			authSecretName    = "test-auth-secret"
			orgID             = "11111111-1111-1111-1111-111111111111"
			clientID          = "test-client"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}
		clavexclient := &clavexv1alpha1.ClavexClient{}
		var fakeAPI *fakeAdminAPI
		var controllerReconciler *ClavexClientReconciler

		BeforeEach(func() {
			fakeAPI = newFakeAdminAPI(orgID, clientID)
			controllerReconciler = &ClavexClientReconciler{
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
					authsecret.DefaultAPIKeySecretKey: testAPIKeyHeaderValue,
					authsecret.OrgIDSecretKey:         orgID,
				},
			}
			Expect(k8sClient.Create(ctx, authSecret)).To(Succeed())

			By("creating the custom resource for the Kind ClavexClient")
			err := k8sClient.Get(ctx, typeNamespacedName, clavexclient)
			if err != nil && errors.IsNotFound(err) {
				resource := &clavexv1alpha1.ClavexClient{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: resourceNamespace,
					},
					Spec: clavexv1alpha1.ClavexClientSpec{
						OrgRef: testOrgRef,
						AuthSecretRef: clavexv1alpha1.SecretRef{
							Name: authSecretName,
						},
						ClientID:     clientID,
						Name:         testClientName,
						RedirectURIs: []string{testClientRedirectURI},
						GrantTypes:   []string{testClientGrantType},
						IsActive:     true,
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			By("Cleanup the specific resource instance ClavexClient")
			resource := &clavexv1alpha1.ClavexClient{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			// No manager/controller is running in the background during
			// these tests, so the finalizer added by Reconcile is never
			// removed automatically. Drive one more reconcile pass here to
			// let the reconciler process the deletion (remove the remote
			// client + finalizer) — otherwise the CR would leak into the
			// next spec with a stale DeletionTimestamp still set.
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
			// First reconcile: adds the finalizer, then re-fetches — the
			// finalizer update bumps resourceVersion but not generation, so
			// a single call already proceeds to create the remote client.
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the status reflects a successful sync")
			resource := &clavexv1alpha1.ClavexClient{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			Expect(resource.Status.ClavexClientID).To(Equal(clientID))
			Expect(resource.Status.Conditions).ToNot(BeEmpty())

			var readyCond *metav1.Condition
			for i := range resource.Status.Conditions {
				if resource.Status.Conditions[i].Type == conditionTypeReady {
					readyCond = &resource.Status.Conditions[i]
				}
			}
			Expect(readyCond).ToNot(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))

			By("verifying no clientSecretRef Secret was created when the field is unset")
			clientSecret := &corev1.Secret{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "unused", Namespace: resourceNamespace}, clientSecret)
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})

		It("should detect and correct out-of-band drift without a spec change", func() {
			By("reconciling once to create the remote client")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("simulating an out-of-band edit directly against the Admin API")
			fakeAPI.liveName = "Edited Outside Kubernetes"
			fakeAPI.patchCalled = false

			By("reconciling again with no spec change")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the drift was corrected via PATCH")
			Expect(fakeAPI.patchCalled).To(BeTrue())
			Expect(fakeAPI.liveName).To(Equal(testClientName))

			By("verifying the Synced condition reports DriftCorrected")
			resource := &clavexv1alpha1.ClavexClient{}
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
