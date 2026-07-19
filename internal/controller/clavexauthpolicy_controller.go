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
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
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

const (
	// clavexAuthPolicyFinalizer ensures the remote policy rule is
	// deleted from the Admin API before the CR is removed from the
	// cluster.
	clavexAuthPolicyFinalizer = "clavex.eu/clavexauthpolicy-finalizer"

	// eventReasonAuthPolicyDriftDetected mirrors eventReasonDriftDetected
	// for ClavexClient — see reconcileAuthPolicy for when it fires.
	eventReasonAuthPolicyDriftDetected = "DriftDetected"
)

// ClavexAuthPolicyReconciler reconciles a ClavexAuthPolicy object
type ClavexAuthPolicyReconciler struct {
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

// streamItems lists all ClavexAuthPolicy CRs as event-stream items.
func (r *ClavexAuthPolicyReconciler) streamItems(ctx context.Context) ([]eventstream.Item, error) {
	var list clavexv1alpha1.ClavexAuthPolicyList
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

// +kubebuilder:rbac:groups=clavex.clavex.eu,resources=clavexauthpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=clavex.clavex.eu,resources=clavexauthpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=clavex.clavex.eu,resources=clavexauthpolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile moves the live Clavex auth-flow policy rule closer to the
// desired state described by a ClavexAuthPolicy CR, using an org-scoped
// Admin API key read from spec.authSecretRef. As with
// ClavexIdentityProvider, there is no client-supplied stable ID: existing
// rules are looked up by spec.name. Unlike ClavexRole, the Admin API's
// PolicyService exposes a full Update, so an existing rule is kept in
// sync with every reconcile rather than treated as immutable.
func (r *ClavexAuthPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var cr clavexv1alpha1.ClavexAuthPolicy
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting ClavexAuthPolicy: %w", err)
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
		if controllerutil.ContainsFinalizer(&cr, clavexAuthPolicyFinalizer) {
			existing, findErr := findAuthPolicyByName(ctx, cvx, orgID, cr.Spec.Name)
			if findErr != nil {
				log.Error(findErr, "Failed to look up policy rule before delete", "name", cr.Spec.Name)
				return ctrl.Result{}, findErr
			}
			if existing != nil {
				if err := cvx.Policies.Delete(ctx, orgID, existing.ID); err != nil && !clavex.IsNotFound(err) {
					log.Error(err, "Failed to delete Clavex policy rule", "name", cr.Spec.Name)
					return ctrl.Result{}, err
				}
				log.Info("Deleted Clavex policy rule", "name", cr.Spec.Name, "org", orgID)
			}
			controllerutil.RemoveFinalizer(&cr, clavexAuthPolicyFinalizer)
			if err := r.Update(ctx, &cr); err != nil {
				return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
			}
		}
		return ctrl.Result{}, nil
	}

	// Ensure finalizer is present before we create anything remotely.
	if !controllerutil.ContainsFinalizer(&cr, clavexAuthPolicyFinalizer) {
		controllerutil.AddFinalizer(&cr, clavexAuthPolicyFinalizer)
		if err := r.Update(ctx, &cr); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
			return ctrl.Result{}, fmt.Errorf("re-fetching ClavexAuthPolicy after finalizer update: %w", err)
		}
	}

	ruleID, drifted, err := r.reconcileAuthPolicy(ctx, cvx, orgID, &cr)
	if err != nil {
		log.Error(err, "Failed to reconcile Clavex policy rule", "name", cr.Spec.Name)
		r.setCondition(&cr, conditionTypeReady, metav1.ConditionFalse, "ReconcileError", err.Error())
		r.setCondition(&cr, conditionTypeSynced, metav1.ConditionFalse, "ReconcileError", err.Error())
		if statusErr := r.Status().Update(ctx, &cr); statusErr != nil {
			log.Error(statusErr, "Failed to update status after reconcile error")
		}
		return ctrl.Result{}, err
	}

	syncReason, syncMessage := syncReasonSynced, "Policy rule is in sync with the Admin API"
	if drifted {
		syncReason, syncMessage = syncReasonDriftCorrected, "Live policy rule had diverged from spec out-of-band; corrected to match spec"
	}

	now := metav1.Now()
	cr.Status.ObservedGeneration = cr.Generation
	cr.Status.ClavexAuthPolicyID = ruleID
	cr.Status.LastSyncedAt = &now
	r.setCondition(&cr, conditionTypeReady, metav1.ConditionTrue, syncReasonSynced, "Policy rule is in sync with the Admin API")
	r.setCondition(&cr, conditionTypeSynced, metav1.ConditionTrue, syncReason, syncMessage)
	if err := r.Status().Update(ctx, &cr); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	log.Info("Reconciled Clavex policy rule", "name", cr.Spec.Name, "org", orgID)
	return ctrl.Result{RequeueAfter: requeueInterval}, nil
}

// findAuthPolicyByName lists policy rules in orgID and returns the one
// whose Name matches, or nil if none does. This is the only way to
// reconcile by a stable key since the Admin API assigns its own opaque
// ID on creation.
func findAuthPolicyByName(ctx context.Context, cvx *clavex.Client, orgID, name string) (*clavex.PolicyRule, error) {
	rules, err := cvx.Policies.List(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("listing policy rules: %w", err)
	}
	for i := range rules {
		if rules[i].Name == name {
			return &rules[i], nil
		}
	}
	return nil, nil
}

// buildAuthPolicyConditions maps the CRD's AuthPolicyConditions onto the
// SDK's PolicyConditions.
func buildAuthPolicyConditions(spec clavexv1alpha1.AuthPolicyConditions) clavex.PolicyConditions {
	cond := clavex.PolicyConditions{
		IPCIDRs:         spec.IPCIDRs,
		Countries:       spec.Countries,
		NotCountries:    spec.NotCountries,
		ClientIDs:       spec.ClientIDs,
		MFAEnrolled:     spec.MFAEnrolled,
		NewCountry:      spec.NewCountry,
		DaysOfWeek:      spec.DaysOfWeek,
		LastLoginBefore: spec.LastLoginBefore,
	}
	if spec.HourRange != nil {
		cond.HourRange = &clavex.HourRange{From: spec.HourRange.From, To: spec.HourRange.To}
	}
	return cond
}

// reconcileAuthPolicy creates or updates the remote policy rule to match
// spec, returning its Admin API ID and whether the update (if any) was
// due to out-of-band drift rather than a legitimate spec change.
func (r *ClavexAuthPolicyReconciler) reconcileAuthPolicy(ctx context.Context, cvx *clavex.Client, orgID string, cr *clavexv1alpha1.ClavexAuthPolicy) (ruleID string, drifted bool, err error) {
	existing, err := findAuthPolicyByName(ctx, cvx, orgID, cr.Spec.Name)
	if err != nil {
		return "", false, err
	}

	conditions := buildAuthPolicyConditions(cr.Spec.Conditions)

	if existing == nil {
		created, err := cvx.Policies.Create(ctx, orgID, clavex.CreatePolicyRuleParams{
			Name:       cr.Spec.Name,
			Priority:   cr.Spec.Priority,
			Action:     cr.Spec.Action,
			Conditions: conditions,
		})
		if err != nil {
			return "", false, fmt.Errorf("creating policy rule: %w", err)
		}
		return created.ID, false, nil
	}

	if authPolicyUpToDate(existing, cr) {
		return existing.ID, false, nil
	}

	drifted = cr.Status.ObservedGeneration == cr.Generation
	if drifted && r.Recorder != nil {
		r.Recorder.Eventf(cr, corev1.EventTypeNormal, eventReasonAuthPolicyDriftDetected,
			"Live policy rule %q no longer matches spec though it was not edited via kubectl; correcting via PUT", cr.Spec.Name)
	}

	name := cr.Spec.Name
	priority := cr.Spec.Priority
	action := cr.Spec.Action
	enabled := cr.Spec.Enabled
	if _, err := cvx.Policies.Update(ctx, orgID, existing.ID, clavex.UpdatePolicyRuleParams{
		Name:       &name,
		Priority:   &priority,
		Action:     &action,
		Enabled:    &enabled,
		Conditions: conditions,
	}); err != nil {
		return "", drifted, fmt.Errorf("updating policy rule: %w", err)
	}
	return existing.ID, drifted, nil
}

// authPolicyUpToDate reports whether the live policy rule already
// matches the mutable fields of spec, avoiding a no-op PUT on every
// reconcile.
func authPolicyUpToDate(live *clavex.PolicyRule, cr *clavexv1alpha1.ClavexAuthPolicy) bool {
	if live.Name != cr.Spec.Name ||
		live.Priority != cr.Spec.Priority ||
		live.Action != cr.Spec.Action ||
		live.Enabled != cr.Spec.Enabled {
		return false
	}
	return reflect.DeepEqual(live.Conditions, buildAuthPolicyConditions(cr.Spec.Conditions))
}

// setCondition upserts a status condition on a ClavexAuthPolicy — see
// ClavexClientReconciler.setCondition for the shared convention.
func (r *ClavexAuthPolicyReconciler) setCondition(cr *clavexv1alpha1.ClavexAuthPolicy, condType string, status metav1.ConditionStatus, reason, message string) {
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
func (r *ClavexAuthPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&clavexv1alpha1.ClavexAuthPolicy{}).
		Named("clavexauthpolicy")
	if r.EventStream != nil {
		b = b.WatchesRawSource(streamSource(r.EventStream, eventstream.ResourceAuthPolicy, r.streamItems))
	}
	return b.Complete(r)
}
