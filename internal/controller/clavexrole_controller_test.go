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
	testRoleName        = "billing-admin"
	testRoleDescription = "Can manage billing settings"
	testRoleAPIKey      = "clv_test-org-scoped-role-key"
)

// fakeRoleAdminAPI is a minimal httptest-backed stand-in for the Clavex
// Admin API's role endpoints. Roles are looked up by name (List) and are
// otherwise immutable, so only List/Create/Delete need to be modelled.
type fakeRoleAdminAPI struct {
	*httptest.Server
	roleID     string
	created    bool
	createHits int
	deleteHits int
}

func newFakeRoleAdminAPI(orgID string) *fakeRoleAdminAPI {
	f := &fakeRoleAdminAPI{roleID: "33333333-3333-3333-3333-333333333333"}
	mux := http.NewServeMux()
	base := "/api/v1/organizations/" + orgID + "/roles"

	rolePayload := func() map[string]any {
		return map[string]any{
			"id":           f.roleID,
			jsonFieldOrgID: orgID,
			jsonFieldName:  testRoleName,
			"description":  testRoleDescription,
			"is_system":    false,
		}
	}

	mux.HandleFunc(base, func(w http.ResponseWriter, r *http.Request) {
		Expect(r.Header.Get("X-API-Key")).To(Equal(testRoleAPIKey))
		switch r.Method {
		case http.MethodGet:
			if !f.created {
				_ = json.NewEncoder(w).Encode([]map[string]any{})
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{rolePayload()})
		case http.MethodPost:
			f.created = true
			f.createHits++
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(rolePayload())
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc(base+"/"+f.roleID, func(w http.ResponseWriter, r *http.Request) {
		Expect(r.Header.Get("X-API-Key")).To(Equal(testRoleAPIKey))
		switch r.Method {
		case http.MethodDelete:
			f.deleteHits++
			f.created = false
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	f.Server = httptest.NewServer(mux)
	return f
}

var _ = Describe("ClavexRole Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-role-resource"
			resourceNamespace = "default"
			authSecretName    = "test-role-auth-secret"
			orgID             = "11111111-1111-1111-1111-111111111111"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}
		clavexrole := &clavexv1alpha1.ClavexRole{}
		var fakeAPI *fakeRoleAdminAPI
		var controllerReconciler *ClavexRoleReconciler

		BeforeEach(func() {
			fakeAPI = newFakeRoleAdminAPI(orgID)
			controllerReconciler = &ClavexRoleReconciler{
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
					authsecret.DefaultAPIKeySecretKey: testRoleAPIKey,
					authsecret.OrgIDSecretKey:         orgID,
				},
			}
			Expect(k8sClient.Create(ctx, authSecret)).To(Succeed())

			By("creating the custom resource for the Kind ClavexRole")
			err := k8sClient.Get(ctx, typeNamespacedName, clavexrole)
			if err != nil && errors.IsNotFound(err) {
				resource := &clavexv1alpha1.ClavexRole{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: resourceNamespace,
					},
					Spec: clavexv1alpha1.ClavexRoleSpec{
						OrgRef: testOrgRef,
						AuthSecretRef: clavexv1alpha1.SecretRef{
							Name: authSecretName,
						},
						Name:        testRoleName,
						Description: testRoleDescription,
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			By("Cleanup the specific resource instance ClavexRole")
			resource := &clavexv1alpha1.ClavexRole{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			// No manager/controller is running in the background during
			// these tests, so drive one more reconcile pass to let the
			// finalizer be processed and removed (see the equivalent note
			// in clavexclient_controller_test.go).
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
			resource := &clavexv1alpha1.ClavexRole{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			Expect(resource.Status.ClavexRoleID).To(Equal(fakeAPI.roleID))
			Expect(resource.Status.Conditions).ToNot(BeEmpty())

			var readyCond *metav1.Condition
			for i := range resource.Status.Conditions {
				if resource.Status.Conditions[i].Type == conditionTypeReady {
					readyCond = &resource.Status.Conditions[i]
				}
			}
			Expect(readyCond).ToNot(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
		})

		It("should not attempt to recreate a role that already exists", func() {
			By("reconciling twice")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the role is still resolved to the same ID (no duplicate create)")
			resource := &clavexv1alpha1.ClavexRole{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			Expect(resource.Status.ClavexRoleID).To(Equal(fakeAPI.roleID))
			Expect(fakeAPI.createHits).To(Equal(1))
		})
	})
})
