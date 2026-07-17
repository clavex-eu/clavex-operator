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
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	clavex "github.com/clavex-eu/clavex-sdk-go"

	clavexv1alpha1 "github.com/clavex-eu/clavex-operator/api/v1alpha1"
	"github.com/clavex-eu/clavex-operator/internal/authsecret"
	"github.com/clavex-eu/clavex-operator/internal/eventstream"
)

// ClavexOrgReconciler reconciles a ClavexOrg object
type ClavexOrgReconciler struct {
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

// streamItems lists all ClavexOrg CRs as event-stream items.
func (r *ClavexOrgReconciler) streamItems(ctx context.Context) ([]eventstream.Item, error) {
	var list clavexv1alpha1.ClavexOrgList
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

// +kubebuilder:rbac:groups=clavex.clavex.eu,resources=clavexorgs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=clavex.clavex.eu,resources=clavexorgs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=clavex.clavex.eu,resources=clavexorgs/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile keeps an existing Clavex organisation's settings (password
// policy, rate limits) in sync with a ClavexOrg CR's spec, using an
// org-scoped Admin API key read from spec.authSecretRef. Unlike the other
// CRDs in this operator, there is no remote object to create or delete —
// every org already has a password policy and rate-limit configuration
// (defaults apply until overridden) — so ClavexOrg has no finalizer:
// deleting the CR simply stops management and leaves the live settings
// as they last were, it does not reset them to platform defaults.
func (r *ClavexOrgReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var cr clavexv1alpha1.ClavexOrg
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting ClavexOrg: %w", err)
	}

	// Nothing to clean up remotely on delete — see the Reconcile doc
	// comment above.
	if !cr.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
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

	cvx, err := clavex.New(r.ClavexServerURL, clavex.WithAPIKey(apiKey))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("building Clavex client: %w", err)
	}

	drifted, err := r.reconcileOrgSettings(ctx, cvx, orgID, &cr)
	if err != nil {
		log.Error(err, "Failed to reconcile Clavex org settings", "org", orgID)
		r.setCondition(&cr, conditionTypeReady, metav1.ConditionFalse, "ReconcileError", err.Error())
		r.setCondition(&cr, conditionTypeSynced, metav1.ConditionFalse, "ReconcileError", err.Error())
		if statusErr := r.Status().Update(ctx, &cr); statusErr != nil {
			log.Error(statusErr, "Failed to update status after reconcile error")
		}
		return ctrl.Result{}, err
	}

	syncReason, syncMessage := syncReasonSynced, "Org settings are in sync with the Admin API"
	if drifted {
		syncReason, syncMessage = syncReasonDriftCorrected, "Live org settings had diverged from spec out-of-band; corrected to match spec"
		if r.Recorder != nil {
			r.Recorder.Event(&cr, "Normal", "DriftDetected", syncMessage)
		}
	}

	now := metav1.Now()
	cr.Status.ObservedGeneration = cr.Generation
	cr.Status.LastSyncedAt = &now
	r.setCondition(&cr, conditionTypeReady, metav1.ConditionTrue, syncReason, syncMessage)
	r.setCondition(&cr, conditionTypeSynced, metav1.ConditionTrue, syncReason, syncMessage)
	if err := r.Status().Update(ctx, &cr); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	log.Info("Reconciled Clavex org settings", "org", orgID)
	return ctrl.Result{RequeueAfter: requeueInterval}, nil
}

// reconcileOrgSettings converges each configured settings section
// (PasswordPolicy, RateLimits) toward spec, reporting whether either
// section had drifted from the live value. Sections left nil in spec are
// left entirely unmanaged.
func (r *ClavexOrgReconciler) reconcileOrgSettings(ctx context.Context, cvx *clavex.Client, orgID string, cr *clavexv1alpha1.ClavexOrg) (drifted bool, err error) {
	if cr.Spec.PasswordPolicy != nil {
		d, err := r.reconcilePasswordPolicy(ctx, cvx, orgID, cr.Spec.PasswordPolicy)
		if err != nil {
			return false, fmt.Errorf("reconciling password policy: %w", err)
		}
		drifted = drifted || d
	}

	if cr.Spec.RateLimits != nil {
		d, err := r.reconcileRateLimits(ctx, cvx, orgID, cr.Spec.RateLimits)
		if err != nil {
			return false, fmt.Errorf("reconciling rate limits: %w", err)
		}
		drifted = drifted || d
	}

	return drifted, nil
}

// reconcilePasswordPolicy fetches the live password policy and, if it
// doesn't match spec, calls Put to converge — reporting whether a live
// value had diverged (as opposed to this being the first reconcile).
func (r *ClavexOrgReconciler) reconcilePasswordPolicy(ctx context.Context, cvx *clavex.Client, orgID string, spec *clavexv1alpha1.PasswordPolicySpec) (drifted bool, err error) {
	live, err := cvx.PasswordPolicy.Get(ctx, orgID)
	if err != nil {
		return false, fmt.Errorf("getting password policy: %w", err)
	}

	desired := clavex.PasswordPolicy{
		OrgID:          orgID,
		MinLength:      spec.MinLength,
		RequireUpper:   spec.RequireUpper,
		RequireLower:   spec.RequireLower,
		RequireNumber:  spec.RequireNumber,
		RequireSpecial: spec.RequireSpecial,
		MaxAgeDays:     spec.MaxAgeDays,
		HistoryCount:   spec.HistoryCount,
	}

	if live != nil && passwordPolicyUpToDate(*live, desired) {
		return false, nil
	}

	if _, err := cvx.PasswordPolicy.Put(ctx, orgID, desired); err != nil {
		return false, fmt.Errorf("updating password policy: %w", err)
	}
	return true, nil
}

// reconcileRateLimits fetches the live rate-limit config and, if it
// doesn't match spec, calls Update to converge.
func (r *ClavexOrgReconciler) reconcileRateLimits(ctx context.Context, cvx *clavex.Client, orgID string, spec *clavexv1alpha1.RateLimitsSpec) (drifted bool, err error) {
	live, err := cvx.RateLimits.Get(ctx, orgID)
	if err != nil {
		return false, fmt.Errorf("getting rate limits: %w", err)
	}

	desired := clavex.RateLimitConfig{
		OrgID:                  orgID,
		MaxAttemptsPerMinute:   spec.MaxAttemptsPerMinute,
		LockoutDurationSeconds: spec.LockoutDurationSeconds,
		IPMaxAttemptsPerMinute: spec.IPMaxAttemptsPerMinute,
	}

	if live != nil && rateLimitsUpToDate(*live, desired) {
		return false, nil
	}

	if _, err := cvx.RateLimits.Update(ctx, orgID, desired); err != nil {
		return false, fmt.Errorf("updating rate limits: %w", err)
	}
	return true, nil
}

// passwordPolicyUpToDate compares the fields ClavexOrg manages, ignoring
// the server-assigned OrgID.
func passwordPolicyUpToDate(live, desired clavex.PasswordPolicy) bool {
	return live.MinLength == desired.MinLength &&
		live.RequireUpper == desired.RequireUpper &&
		live.RequireLower == desired.RequireLower &&
		live.RequireNumber == desired.RequireNumber &&
		live.RequireSpecial == desired.RequireSpecial &&
		live.MaxAgeDays == desired.MaxAgeDays &&
		live.HistoryCount == desired.HistoryCount
}

// rateLimitsUpToDate compares the fields ClavexOrg manages, ignoring the
// server-assigned OrgID.
func rateLimitsUpToDate(live, desired clavex.RateLimitConfig) bool {
	return live.MaxAttemptsPerMinute == desired.MaxAttemptsPerMinute &&
		live.LockoutDurationSeconds == desired.LockoutDurationSeconds &&
		live.IPMaxAttemptsPerMinute == desired.IPMaxAttemptsPerMinute
}

// setCondition upserts a status condition on a ClavexOrg — see
// ClavexClientReconciler.setCondition for the shared convention.
func (r *ClavexOrgReconciler) setCondition(cr *clavexv1alpha1.ClavexOrg, condType string, status metav1.ConditionStatus, reason, message string) {
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
func (r *ClavexOrgReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&clavexv1alpha1.ClavexOrg{}).
		Named("clavexorg")
	if r.EventStream != nil {
		b = b.WatchesRawSource(streamSource(r.EventStream, eventstream.ResourceOrg, r.streamItems))
	}
	return b.Complete(r)
}
