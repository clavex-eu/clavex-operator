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
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	clavex "github.com/clavex-eu/clavex-sdk-go"

	clavexv1alpha1 "github.com/clavex-eu/clavex-operator/api/v1alpha1"
	"github.com/clavex-eu/clavex-operator/internal/authsecret"
	"github.com/clavex-eu/clavex-operator/internal/eventstream"
)

// clavexRoleFinalizer ensures the remote role is deleted from the Admin
// API before the CR is removed from the cluster.
const clavexRoleFinalizer = "clavex.eu/clavexrole-finalizer"

// ClavexRoleReconciler reconciles a ClavexRole object
type ClavexRoleReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Recorder emits Kubernetes Events — see ClavexClientReconciler.Recorder.
	Recorder record.EventRecorder

	// ClavexServerURL is the base URL of the Clavex Admin API. See
	// ClavexClientReconciler.ClavexServerURL.
	ClavexServerURL string

	// EventStream drives near-instant drift detection — see
	// ClavexClientReconciler.EventStream.
	EventStream *eventstream.Manager
}

// streamItems lists all ClavexRole CRs as event-stream items.
func (r *ClavexRoleReconciler) streamItems(ctx context.Context) ([]eventstream.Item, error) {
	var list clavexv1alpha1.ClavexRoleList
	if err := r.List(ctx, &list); err != nil {
		return nil, err
	}
	items := make([]eventstream.Item, 0, len(list.Items))
	for i := range list.Items {
		cr := &list.Items[i]
		items = append(items, eventstream.Item{
			Object:    cr,
			OrgSlug:   cr.Spec.OrgRef,
			SecretRef: cr.Spec.AuthSecretRef,
			Namespace: cr.Namespace,
		})
	}
	return items, nil
}

// +kubebuilder:rbac:groups=clavex.clavex.eu,resources=clavexroles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=clavex.clavex.eu,resources=clavexroles/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=clavex.clavex.eu,resources=clavexroles/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile moves the live Clavex role closer to the desired state
// described by a ClavexRole CR, using an org-scoped Admin API key read
// from spec.authSecretRef. Roles are looked up by spec.name (there is no
// client-supplied stable ID) and, once created, are treated as immutable
// — the Admin API's RoleService exposes no Update, only Create/Delete —
// so a spec change to Name or Description after creation is a no-op
// against the live resource (mirrors clavexctl org apply's documented
// "Roles have no updateable fields beyond name so skip update").
func (r *ClavexRoleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var cr clavexv1alpha1.ClavexRole
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting ClavexRole: %w", err)
	}

	apiKey, orgID, err := authsecret.ResolveAuthSecret(ctx, r.Client, cr.Spec.AuthSecretRef, cr.Namespace)
	if err != nil {
		log.Error(err, "Failed to resolve auth secret")
		r.setCondition(&cr, conditionTypeReady, metav1.ConditionFalse, "AuthSecretError", err.Error())
		if statusErr := r.Status().Update(ctx, &cr); statusErr != nil {
			log.Error(statusErr, "Failed to update status after auth error")
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if r.EventStream != nil {
		r.EventStream.EnsureOrgWithKey(cr.Spec.OrgRef, apiKey)
	}
	cvx, err := clavex.New(r.ClavexServerURL, clavex.WithAPIKey(apiKey))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("building Clavex client: %w", err)
	}

	// Handle deletion.
	if !cr.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&cr, clavexRoleFinalizer) {
			existing, findErr := findRoleByName(ctx, cvx, orgID, cr.Spec.Name)
			if findErr != nil {
				log.Error(findErr, "Failed to look up role before delete", "name", cr.Spec.Name)
				return ctrl.Result{}, findErr
			}
			if existing != nil {
				if err := cvx.Roles.Delete(ctx, orgID, existing.ID); err != nil && !clavex.IsNotFound(err) {
					log.Error(err, "Failed to delete Clavex role", "name", cr.Spec.Name)
					return ctrl.Result{}, err
				}
				log.Info("Deleted Clavex role", "name", cr.Spec.Name, "org", orgID)
			}
			controllerutil.RemoveFinalizer(&cr, clavexRoleFinalizer)
			if err := r.Update(ctx, &cr); err != nil {
				return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
			}
		}
		return ctrl.Result{}, nil
	}

	// Ensure finalizer is present before we create anything remotely.
	if !controllerutil.ContainsFinalizer(&cr, clavexRoleFinalizer) {
		controllerutil.AddFinalizer(&cr, clavexRoleFinalizer)
		if err := r.Update(ctx, &cr); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
			return ctrl.Result{}, fmt.Errorf("re-fetching ClavexRole after finalizer update: %w", err)
		}
	}

	roleID, err := r.reconcileRole(ctx, cvx, orgID, &cr)
	if err != nil {
		log.Error(err, "Failed to reconcile Clavex role", "name", cr.Spec.Name)
		r.setCondition(&cr, conditionTypeReady, metav1.ConditionFalse, "ReconcileError", err.Error())
		r.setCondition(&cr, conditionTypeSynced, metav1.ConditionFalse, "ReconcileError", err.Error())
		if statusErr := r.Status().Update(ctx, &cr); statusErr != nil {
			log.Error(statusErr, "Failed to update status after reconcile error")
		}
		return ctrl.Result{}, err
	}

	now := metav1.Now()
	cr.Status.ObservedGeneration = cr.Generation
	cr.Status.ClavexRoleID = roleID
	cr.Status.LastSyncedAt = &now
	r.setCondition(&cr, conditionTypeReady, metav1.ConditionTrue, syncReasonSynced, "Role is in sync with the Admin API")
	r.setCondition(&cr, conditionTypeSynced, metav1.ConditionTrue, syncReasonSynced, "Role is in sync with the Admin API")
	if err := r.Status().Update(ctx, &cr); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	log.Info("Reconciled Clavex role", "name", cr.Spec.Name, "org", orgID)
	return ctrl.Result{RequeueAfter: requeueInterval}, nil
}

// findRoleByName lists roles in orgID and returns the one whose Name
// matches, or nil if none does.
func findRoleByName(ctx context.Context, cvx *clavex.Client, orgID, name string) (*clavex.Role, error) {
	roles, err := cvx.Roles.List(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("listing roles: %w", err)
	}
	for i := range roles {
		if roles[i].Name == name {
			return &roles[i], nil
		}
	}
	return nil, nil
}

// reconcileRole creates the remote role if it doesn't already exist. Since
// the Admin API has no role-update endpoint, an existing role is left
// untouched even if Description has drifted from spec — see the Reconcile
// doc comment for details.
func (r *ClavexRoleReconciler) reconcileRole(ctx context.Context, cvx *clavex.Client, orgID string, cr *clavexv1alpha1.ClavexRole) (roleID string, err error) {
	existing, err := findRoleByName(ctx, cvx, orgID, cr.Spec.Name)
	if err != nil {
		return "", err
	}
	if existing != nil {
		return existing.ID, nil
	}

	created, err := cvx.Roles.Create(ctx, orgID, clavex.CreateRoleParams{
		Name:        cr.Spec.Name,
		Description: cr.Spec.Description,
	})
	if err != nil {
		return "", fmt.Errorf("creating role: %w", err)
	}
	return created.ID, nil
}

// setCondition upserts a status condition on a ClavexRole — see
// ClavexClientReconciler.setCondition for the shared convention.
func (r *ClavexRoleReconciler) setCondition(cr *clavexv1alpha1.ClavexRole, condType string, status metav1.ConditionStatus, reason, message string) {
	newCond := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: cr.Generation,
		LastTransitionTime: metav1.Now(),
	}
	for i, existing := range cr.Status.Conditions {
		if existing.Type == condType {
			if existing.Status == status {
				newCond.LastTransitionTime = existing.LastTransitionTime
			}
			cr.Status.Conditions[i] = newCond
			return
		}
	}
	cr.Status.Conditions = append(cr.Status.Conditions, newCond)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClavexRoleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&clavexv1alpha1.ClavexRole{}).
		Named("clavexrole")
	if r.EventStream != nil {
		b = b.WatchesRawSource(streamSource(r.EventStream, eventstream.ResourceRole, r.streamItems))
	}
	return b.Complete(r)
}
