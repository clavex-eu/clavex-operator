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
	"slices"
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
	// clavexWebhookFinalizer ensures the remote webhook is deleted from
	// the Admin API before the CR is removed from the cluster.
	clavexWebhookFinalizer = "clavex.eu/clavexwebhook-finalizer"

	// eventReasonWebhookDriftDetected mirrors eventReasonDriftDetected —
	// see reconcileWebhook for when it fires.
	eventReasonWebhookDriftDetected = "DriftDetected"
)

// ClavexWebhookReconciler reconciles a ClavexWebhook object
type ClavexWebhookReconciler struct {
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

// streamItems lists all ClavexWebhook CRs as event-stream items.
func (r *ClavexWebhookReconciler) streamItems(ctx context.Context) ([]eventstream.Item, error) {
	var list clavexv1alpha1.ClavexWebhookList
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

// +kubebuilder:rbac:groups=clavex.clavex.eu,resources=clavexwebhooks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=clavex.clavex.eu,resources=clavexwebhooks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=clavex.clavex.eu,resources=clavexwebhooks/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile moves the live Clavex webhook closer to the desired state
// described by a ClavexWebhook CR, using an org-scoped Admin API key read
// from spec.authSecretRef. Webhooks are looked up by spec.url — see
// ClavexWebhookSpec's doc comment for why url is used instead of name
// (the Admin API's webhooks resource has no name field at all).
func (r *ClavexWebhookReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var cr clavexv1alpha1.ClavexWebhook
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting ClavexWebhook: %w", err)
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
		if controllerutil.ContainsFinalizer(&cr, clavexWebhookFinalizer) {
			existing, findErr := findWebhookByURL(ctx, cvx, orgID, cr.Spec.URL)
			if findErr != nil {
				log.Error(findErr, "Failed to look up webhook before delete", "url", cr.Spec.URL)
				return ctrl.Result{}, findErr
			}
			if existing != nil {
				if err := cvx.Webhooks.Delete(ctx, orgID, existing.ID); err != nil && !clavex.IsNotFound(err) {
					log.Error(err, "Failed to delete Clavex webhook", "url", cr.Spec.URL)
					return ctrl.Result{}, err
				}
				log.Info("Deleted Clavex webhook", "url", cr.Spec.URL, "org", orgID)
			}
			controllerutil.RemoveFinalizer(&cr, clavexWebhookFinalizer)
			if err := r.Update(ctx, &cr); err != nil {
				return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
			}
		}
		return ctrl.Result{}, nil
	}

	// Ensure finalizer is present before we create anything remotely.
	if !controllerutil.ContainsFinalizer(&cr, clavexWebhookFinalizer) {
		controllerutil.AddFinalizer(&cr, clavexWebhookFinalizer)
		if err := r.Update(ctx, &cr); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
			return ctrl.Result{}, fmt.Errorf("re-fetching ClavexWebhook after finalizer update: %w", err)
		}
	}

	webhookID, drifted, err := r.reconcileWebhook(ctx, cvx, orgID, &cr)
	if err != nil {
		log.Error(err, "Failed to reconcile Clavex webhook", "url", cr.Spec.URL)
		r.setCondition(&cr, conditionTypeReady, metav1.ConditionFalse, "ReconcileError", err.Error())
		r.setCondition(&cr, conditionTypeSynced, metav1.ConditionFalse, "ReconcileError", err.Error())
		if statusErr := r.Status().Update(ctx, &cr); statusErr != nil {
			log.Error(statusErr, "Failed to update status after reconcile error")
		}
		return ctrl.Result{}, err
	}

	syncReason, syncMessage := syncReasonSynced, "Webhook is in sync with the Admin API"
	if drifted {
		syncReason, syncMessage = syncReasonDriftCorrected, "Live webhook had diverged from spec out-of-band; corrected to match spec"
	}

	now := metav1.Now()
	cr.Status.ObservedGeneration = cr.Generation
	cr.Status.ClavexWebhookID = webhookID
	cr.Status.LastSyncedAt = &now
	r.setCondition(&cr, conditionTypeReady, metav1.ConditionTrue, syncReasonSynced, "Webhook is in sync with the Admin API")
	r.setCondition(&cr, conditionTypeSynced, metav1.ConditionTrue, syncReason, syncMessage)
	if err := r.Status().Update(ctx, &cr); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	log.Info("Reconciled Clavex webhook", "url", cr.Spec.URL, "org", orgID)
	return ctrl.Result{RequeueAfter: requeueInterval}, nil
}

// findWebhookByURL lists webhooks in orgID and returns the one whose URL
// matches, or nil if none does. See ClavexWebhookSpec's doc comment for
// why URL (not name) is the reconciliation key.
func findWebhookByURL(ctx context.Context, cvx *clavex.Client, orgID, url string) (*clavex.Webhook, error) {
	webhooks, err := cvx.Webhooks.List(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("listing webhooks: %w", err)
	}
	for i := range webhooks {
		if webhooks[i].URL == url {
			return &webhooks[i], nil
		}
	}
	return nil, nil
}

// buildWebhookParams resolves spec + the optional signingKeyRef into the
// CreateWebhookParams shape shared by Create and Update.
func (r *ClavexWebhookReconciler) buildWebhookParams(ctx context.Context, cr *clavexv1alpha1.ClavexWebhook) (clavex.CreateWebhookParams, error) {
	enabled := cr.Spec.Enabled
	params := clavex.CreateWebhookParams{
		URL:      cr.Spec.URL,
		Events:   cr.Spec.Events,
		IsActive: &enabled,
	}
	if cr.Spec.SigningKeyRef != nil {
		secretValue, err := authsecret.ResolveSecretValue(ctx, r.Client, cr.Spec.SigningKeyRef, cr.Namespace, authsecret.DefaultClientSecretKey)
		if err != nil {
			return clavex.CreateWebhookParams{}, fmt.Errorf("resolving signingKeyRef: %w", err)
		}
		params.Secret = secretValue
	}
	return params, nil
}

// reconcileWebhook creates or updates the remote webhook to match spec,
// returning its Admin API ID and whether the update (if any) was due to
// out-of-band drift rather than a legitimate spec change.
func (r *ClavexWebhookReconciler) reconcileWebhook(ctx context.Context, cvx *clavex.Client, orgID string, cr *clavexv1alpha1.ClavexWebhook) (webhookID string, drifted bool, err error) {
	existing, err := findWebhookByURL(ctx, cvx, orgID, cr.Spec.URL)
	if err != nil {
		return "", false, err
	}

	params, err := r.buildWebhookParams(ctx, cr)
	if err != nil {
		return "", false, err
	}

	mctx := withManaged(ctx, "ClavexWebhook", cr)

	if existing == nil {
		created, err := cvx.Webhooks.Create(mctx, orgID, params)
		if err != nil {
			return "", false, fmt.Errorf("creating webhook: %w", err)
		}
		return created.ID, false, nil
	}

	if webhookUpToDate(existing, cr) {
		return existing.ID, false, nil
	}

	drifted = cr.Status.ObservedGeneration == cr.Generation
	if drifted && r.Recorder != nil {
		r.Recorder.Eventf(cr, corev1.EventTypeNormal, eventReasonWebhookDriftDetected,
			"Live webhook %q no longer matches spec though it was not edited via kubectl; correcting via PATCH", cr.Spec.URL)
	}

	if _, err := cvx.Webhooks.Update(mctx, orgID, existing.ID, params); err != nil {
		return "", drifted, fmt.Errorf("updating webhook: %w", err)
	}
	return existing.ID, drifted, nil
}

// webhookUpToDate reports whether the live webhook already matches the
// mutable fields of spec, avoiding a no-op PATCH on every reconcile. The
// signing secret is intentionally excluded from the comparison since the
// Admin API never echoes it back (internal/models.Webhook.Secret is
// tagged json:"-").
func webhookUpToDate(live *clavex.Webhook, cr *clavexv1alpha1.ClavexWebhook) bool {
	if live.URL != cr.Spec.URL || live.IsActive != cr.Spec.Enabled {
		return false
	}
	return slices.Equal(live.Events, cr.Spec.Events)
}

// setCondition upserts a status condition on a ClavexWebhook — see
// ClavexClientReconciler.setCondition for the shared convention.
func (r *ClavexWebhookReconciler) setCondition(cr *clavexv1alpha1.ClavexWebhook, condType string, status metav1.ConditionStatus, reason, message string) {
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
func (r *ClavexWebhookReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&clavexv1alpha1.ClavexWebhook{}).
		Named("clavexwebhook")
	if r.EventStream != nil {
		b = b.WatchesRawSource(streamSource(r.EventStream, eventstream.ResourceWebhook, r.streamItems))
	}
	return b.Complete(r)
}
