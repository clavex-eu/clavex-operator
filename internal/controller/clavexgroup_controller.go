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

// clavexGroupFinalizer ensures the remote group is deleted from the Admin
// API before the CR is removed from the cluster.
const clavexGroupFinalizer = "clavex.eu/clavexgroup-finalizer"

// ClavexGroupReconciler reconciles a ClavexGroup object
type ClavexGroupReconciler struct {
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

// streamItems lists all ClavexGroup CRs as event-stream items.
func (r *ClavexGroupReconciler) streamItems(ctx context.Context) ([]eventstream.Item, error) {
	var list clavexv1alpha1.ClavexGroupList
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

// +kubebuilder:rbac:groups=clavex.clavex.eu,resources=clavexgroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=clavex.clavex.eu,resources=clavexgroups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=clavex.clavex.eu,resources=clavexgroups/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile moves the live Clavex group closer to the desired state
// described by a ClavexGroup CR, using an org-scoped Admin API key read
// from spec.authSecretRef. Groups are looked up by spec.name (there is no
// client-supplied stable ID). Unlike ClavexRole, group role-membership *is*
// actively reconciled on every pass: each name in spec.Roles is resolved to
// a live role ID and the group's assigned roles are converged (via
// AssignRole/RemoveRole) to match exactly.
func (r *ClavexGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var cr clavexv1alpha1.ClavexGroup
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting ClavexGroup: %w", err)
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
		if controllerutil.ContainsFinalizer(&cr, clavexGroupFinalizer) {
			existing, findErr := findGroupByName(ctx, cvx, orgID, cr.Spec.Name)
			if findErr != nil {
				log.Error(findErr, "Failed to look up group before delete", "name", cr.Spec.Name)
				return ctrl.Result{}, findErr
			}
			if existing != nil {
				if err := cvx.Groups.Delete(ctx, orgID, existing.ID); err != nil && !clavex.IsNotFound(err) {
					log.Error(err, "Failed to delete Clavex group", "name", cr.Spec.Name)
					return ctrl.Result{}, err
				}
				log.Info("Deleted Clavex group", "name", cr.Spec.Name, "org", orgID)
			}
			controllerutil.RemoveFinalizer(&cr, clavexGroupFinalizer)
			if err := r.Update(ctx, &cr); err != nil {
				return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
			}
		}
		return ctrl.Result{}, nil
	}

	// Ensure finalizer is present before we create anything remotely.
	if !controllerutil.ContainsFinalizer(&cr, clavexGroupFinalizer) {
		controllerutil.AddFinalizer(&cr, clavexGroupFinalizer)
		if err := r.Update(ctx, &cr); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
			return ctrl.Result{}, fmt.Errorf("re-fetching ClavexGroup after finalizer update: %w", err)
		}
	}

	groupID, err := r.reconcileGroup(ctx, cvx, orgID, &cr)
	if err != nil {
		log.Error(err, "Failed to reconcile Clavex group", "name", cr.Spec.Name)
		r.setCondition(&cr, conditionTypeReady, metav1.ConditionFalse, "ReconcileError", err.Error())
		r.setCondition(&cr, conditionTypeSynced, metav1.ConditionFalse, "ReconcileError", err.Error())
		if statusErr := r.Status().Update(ctx, &cr); statusErr != nil {
			log.Error(statusErr, "Failed to update status after reconcile error")
		}
		// Role names that don't resolve yet are a transient condition
		// (e.g. the ClavexRole hasn't been applied/reconciled yet) —
		// requeue instead of giving up, so ordering between a
		// ClavexGroup and its ClavexRole dependencies doesn't matter.
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	now := metav1.Now()
	cr.Status.ObservedGeneration = cr.Generation
	cr.Status.ClavexGroupID = groupID
	cr.Status.LastSyncedAt = &now
	r.setCondition(&cr, conditionTypeReady, metav1.ConditionTrue, syncReasonSynced, "Group is in sync with the Admin API")
	r.setCondition(&cr, conditionTypeSynced, metav1.ConditionTrue, syncReasonSynced, "Group is in sync with the Admin API")
	if err := r.Status().Update(ctx, &cr); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	log.Info("Reconciled Clavex group", "name", cr.Spec.Name, "org", orgID)
	return ctrl.Result{RequeueAfter: requeueInterval}, nil
}

// findGroupByName lists groups in orgID and returns the one whose Name
// matches, or nil if none does.
func findGroupByName(ctx context.Context, cvx *clavex.Client, orgID, name string) (*clavex.Group, error) {
	groups, err := cvx.Groups.List(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("listing groups: %w", err)
	}
	for i := range groups {
		if groups[i].Name == name {
			return &groups[i], nil
		}
	}
	return nil, nil
}

// reconcileGroup creates the remote group if needed, then converges its
// role membership to exactly spec.Roles (a list of role *names*).
func (r *ClavexGroupReconciler) reconcileGroup(ctx context.Context, cvx *clavex.Client, orgID string, cr *clavexv1alpha1.ClavexGroup) (groupID string, err error) {
	existing, err := findGroupByName(ctx, cvx, orgID, cr.Spec.Name)
	if err != nil {
		return "", err
	}

	var gid string
	if existing == nil {
		created, err := cvx.Groups.Create(withManaged(ctx, "ClavexGroup", cr), orgID, clavex.CreateGroupParams{Name: cr.Spec.Name})
		if err != nil {
			return "", fmt.Errorf("creating group: %w", err)
		}
		gid = created.ID
	} else {
		gid = existing.ID
	}

	if err := r.syncGroupRoles(ctx, cvx, orgID, gid, cr.Spec.Roles); err != nil {
		return gid, err
	}
	return gid, nil
}

// syncGroupRoles resolves each entry of wantRoleNames to a live role ID
// (returning an error if any name doesn't resolve — see the caller's
// requeue-on-error handling) and converges the group's assigned roles to
// match exactly, assigning missing ones and removing extras.
func (r *ClavexGroupReconciler) syncGroupRoles(ctx context.Context, cvx *clavex.Client, orgID, groupID string, wantRoleNames []string) error {
	allRoles, err := cvx.Roles.List(ctx, orgID)
	if err != nil {
		return fmt.Errorf("listing roles: %w", err)
	}
	roleIDByName := make(map[string]string, len(allRoles))
	for _, role := range allRoles {
		roleIDByName[role.Name] = role.ID
	}

	wantIDs := make(map[string]struct{}, len(wantRoleNames))
	for _, name := range wantRoleNames {
		id, ok := roleIDByName[name]
		if !ok {
			return fmt.Errorf("role %q referenced by group is not (yet) present in org %s", name, orgID)
		}
		wantIDs[id] = struct{}{}
	}

	currentRoles, err := cvx.Groups.ListRoles(ctx, orgID, groupID)
	if err != nil {
		return fmt.Errorf("listing group roles: %w", err)
	}
	currentIDs := make(map[string]struct{}, len(currentRoles))
	for _, role := range currentRoles {
		currentIDs[role.ID] = struct{}{}
	}

	for id := range wantIDs {
		if _, ok := currentIDs[id]; !ok {
			if err := cvx.Groups.AssignRole(ctx, orgID, groupID, id); err != nil {
				return fmt.Errorf("assigning role %s to group: %w", id, err)
			}
		}
	}
	for id := range currentIDs {
		if _, ok := wantIDs[id]; !ok {
			if err := cvx.Groups.RemoveRole(ctx, orgID, groupID, id); err != nil {
				return fmt.Errorf("removing role %s from group: %w", id, err)
			}
		}
	}
	return nil
}

// setCondition upserts a status condition on a ClavexGroup — see
// ClavexClientReconciler.setCondition for the shared convention.
func (r *ClavexGroupReconciler) setCondition(cr *clavexv1alpha1.ClavexGroup, condType string, status metav1.ConditionStatus, reason, message string) {
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
func (r *ClavexGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&clavexv1alpha1.ClavexGroup{}).
		Named("clavexgroup")
	if r.EventStream != nil {
		b = b.WatchesRawSource(streamSource(r.EventStream, eventstream.ResourceGroup, r.streamItems))
	}
	return b.Complete(r)
}
