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
	"strings"

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
	testGroupName     = "billing-team"
	testGroupAPIKey   = "clv_test-org-scoped-group-key"
	testGroupRoleName = "billing-admin"
	testGroupRoleID   = "44444444-4444-4444-4444-444444444444"
	testGroupID       = "55555555-5555-5555-5555-555555555555"
)

// fakeGroupAdminAPI is a minimal httptest-backed stand-in for the Clavex
// Admin API's group + role endpoints, enough to drive
// ClavexGroupReconciler through create and role-membership convergence.
type fakeGroupAdminAPI struct {
	*httptest.Server
	groupCreated  bool
	assignedRoles map[string]bool // roleID -> assigned
}

func newFakeGroupAdminAPI(orgID string) *fakeGroupAdminAPI {
	f := &fakeGroupAdminAPI{assignedRoles: map[string]bool{}}
	mux := http.NewServeMux()
	rolesBase := "/api/v1/organizations/" + orgID + "/roles"
	groupsBase := "/api/v1/organizations/" + orgID + "/groups"

	// Roles: a single pre-existing role, "billing-admin", used to resolve
	// spec.Roles entries by name.
	mux.HandleFunc(rolesBase, func(w http.ResponseWriter, r *http.Request) {
		Expect(r.Header.Get("X-API-Key")).To(Equal(testGroupAPIKey))
		Expect(r.Method).To(Equal(http.MethodGet))
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{jsonFieldID: testGroupRoleID, jsonFieldOrgID: orgID, jsonFieldName: testGroupRoleName, "is_system": false},
		})
	})

	groupPayload := func() map[string]any {
		return map[string]any{jsonFieldID: testGroupID, jsonFieldOrgID: orgID, jsonFieldName: testGroupName}
	}

	mux.HandleFunc(groupsBase, func(w http.ResponseWriter, r *http.Request) {
		Expect(r.Header.Get("X-API-Key")).To(Equal(testGroupAPIKey))
		switch r.Method {
		case http.MethodGet:
			if !f.groupCreated {
				_ = json.NewEncoder(w).Encode([]map[string]any{})
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{groupPayload()})
		case http.MethodPost:
			f.groupCreated = true
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(groupPayload())
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc(groupsBase+"/"+testGroupID+"/roles", func(w http.ResponseWriter, r *http.Request) {
		Expect(r.Header.Get("X-API-Key")).To(Equal(testGroupAPIKey))
		Expect(r.Method).To(Equal(http.MethodGet))
		var assigned []map[string]any
		for roleID, ok := range f.assignedRoles {
			if ok {
				assigned = append(assigned, map[string]any{jsonFieldID: roleID, jsonFieldName: testGroupRoleName})
			}
		}
		_ = json.NewEncoder(w).Encode(assigned)
	})

	// PUT/DELETE .../groups/{groupID}/roles/{roleID}
	mux.HandleFunc(groupsBase+"/"+testGroupID+"/roles/", func(w http.ResponseWriter, r *http.Request) {
		Expect(r.Header.Get("X-API-Key")).To(Equal(testGroupAPIKey))
		roleID := strings.TrimPrefix(r.URL.Path, groupsBase+"/"+testGroupID+"/roles/")
		switch r.Method {
		case http.MethodPut:
			f.assignedRoles[roleID] = true
			w.WriteHeader(http.StatusNoContent)
		case http.MethodDelete:
			delete(f.assignedRoles, roleID)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc(groupsBase+"/"+testGroupID, func(w http.ResponseWriter, r *http.Request) {
		Expect(r.Header.Get("X-API-Key")).To(Equal(testGroupAPIKey))
		switch r.Method {
		case http.MethodDelete:
			f.groupCreated = false
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	f.Server = httptest.NewServer(mux)
	return f
}

var _ = Describe("ClavexGroup Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-group-resource"
			resourceNamespace = "default"
			authSecretName    = "test-group-auth-secret"
			orgID             = "11111111-1111-1111-1111-111111111111"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}
		clavexgroup := &clavexv1alpha1.ClavexGroup{}
		var fakeAPI *fakeGroupAdminAPI
		var controllerReconciler *ClavexGroupReconciler

		BeforeEach(func() {
			fakeAPI = newFakeGroupAdminAPI(orgID)
			controllerReconciler = &ClavexGroupReconciler{
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
					authsecret.DefaultAPIKeySecretKey: testGroupAPIKey,
					authsecret.OrgIDSecretKey:         orgID,
				},
			}
			Expect(k8sClient.Create(ctx, authSecret)).To(Succeed())

			By("creating the custom resource for the Kind ClavexGroup")
			err := k8sClient.Get(ctx, typeNamespacedName, clavexgroup)
			if err != nil && errors.IsNotFound(err) {
				resource := &clavexv1alpha1.ClavexGroup{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: resourceNamespace,
					},
					Spec: clavexv1alpha1.ClavexGroupSpec{
						OrgRef: testOrgRef,
						AuthSecretRef: clavexv1alpha1.SecretRef{
							Name: authSecretName,
						},
						Name:  testGroupName,
						Roles: []string{testGroupRoleName},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			By("Cleanup the specific resource instance ClavexGroup")
			resource := &clavexv1alpha1.ClavexGroup{}
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

		It("should successfully reconcile the resource and assign the requested role", func() {
			By("Reconciling the created resource")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the status reflects a successful sync")
			resource := &clavexv1alpha1.ClavexGroup{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			Expect(resource.Status.ClavexGroupID).To(Equal(testGroupID))

			var readyCond *metav1.Condition
			for i := range resource.Status.Conditions {
				if resource.Status.Conditions[i].Type == conditionTypeReady {
					readyCond = &resource.Status.Conditions[i]
				}
			}
			Expect(readyCond).ToNot(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))

			By("verifying the role was assigned to the group")
			Expect(fakeAPI.assignedRoles[testGroupRoleID]).To(BeTrue())
		})

		It("should unassign a role that is removed from spec.Roles", func() {
			By("reconciling once with the role present")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(fakeAPI.assignedRoles[testGroupRoleID]).To(BeTrue())

			By("removing the role from spec.Roles")
			resource := &clavexv1alpha1.ClavexGroup{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			resource.Spec.Roles = nil
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())

			By("reconciling again")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the role was unassigned")
			Expect(fakeAPI.assignedRoles[testGroupRoleID]).To(BeFalse())
		})
	})
})
