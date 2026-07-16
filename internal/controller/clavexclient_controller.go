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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	clavex "github.com/clavex-eu/clavex-sdk-go"

	clavexv1alpha1 "github.com/clavex-eu/clavex-operator/api/v1alpha1"
	"github.com/clavex-eu/clavex-operator/internal/authsecret"
)

const (
	// clavexClientFinalizer ensures the remote OIDC client is deleted from
	// the Admin API before the CR is removed from the cluster.
	clavexClientFinalizer = "clavex.eu/clavexclient-finalizer"

	// requeueInterval drives periodic drift detection: even without a spec
	// change, we re-reconcile periodically to catch out-of-band edits made
	// directly against the Admin API (mirrors `clavexctl org diff`).
	requeueInterval = 5 * time.Minute

	conditionTypeReady  = "Ready"
	conditionTypeSynced = "Synced"

	// syncReasonSynced/syncReasonDriftCorrected are the two Reason values
	// used on the Synced condition by every CRD's reconciler that
	// implements drift detection (ClavexClient, ClavexIdentityProvider,
	// ClavexWebhook, …) — shared here to avoid repeating the literals
	// across each controller file.
	syncReasonSynced         = "Synced"
	syncReasonDriftCorrected = "DriftCorrected"

	// eventReasonDriftDetected is emitted when the live Admin API client
	// no longer matches spec (e.g. edited out-of-band via clavexctl or the
	// Admin UI), just before the reconciler corrects it. This keeps the
	// correction visible instead of a silent overwrite.
	eventReasonDriftDetected = "DriftDetected"
)

// ClavexClientReconciler reconciles a ClavexClient object
type ClavexClientReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Recorder emits Kubernetes Events, used to surface out-of-band drift
	// corrections visibly (kubectl describe / events) rather than silently
	// overwriting the Admin API resource on every reconcile.
	Recorder record.EventRecorder

	// ClavexServerURL is the base URL of the Clavex Admin API (e.g.
	// https://id.acme.example). Cluster-wide install: a single Clavex
	// instance is targeted by this controller-manager; per-org isolation
	// comes from the org-scoped API key in each CR's authSecretRef, not
	// from a per-org server URL.
	ClavexServerURL string
}

// +kubebuilder:rbac:groups=clavex.clavex.eu,resources=clavexclients,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=clavex.clavex.eu,resources=clavexclients/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=clavex.clavex.eu,resources=clavexclients/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile moves the live Clavex OIDC client closer to the desired state
// described by a ClavexClient CR, using an org-scoped Admin API key read
// from spec.authSecretRef.
func (r *ClavexClientReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var cr clavexv1alpha1.ClavexClient
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting ClavexClient: %w", err)
	}

	// Resolve credentials + org ID up front — needed for both delete and
	// create/update paths.
	apiKey, orgID, err := authsecret.ResolveAuthSecret(ctx, r.Client, cr.Spec.AuthSecretRef, cr.Namespace)
	if err != nil {
		log.Error(err, "Failed to resolve auth secret")
		r.setCondition(&cr, conditionTypeReady, metav1.ConditionFalse, "AuthSecretError", err.Error())
		if statusErr := r.Status().Update(ctx, &cr); statusErr != nil {
			log.Error(statusErr, "Failed to update status after auth error")
		}
		// Requeue with backoff — the Secret may not exist yet if CRs and
		// Secrets are applied together in one manifest.
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	cvx, err := clavex.New(r.ClavexServerURL, clavex.WithAPIKey(apiKey))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("building Clavex client: %w", err)
	}

	// Handle deletion.
	if !cr.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&cr, clavexClientFinalizer) {
			if err := cvx.Clients.Delete(ctx, orgID, cr.Spec.ClientID); err != nil && !clavex.IsNotFound(err) {
				log.Error(err, "Failed to delete Clavex client", "clientId", cr.Spec.ClientID)
				return ctrl.Result{}, err
			}
			log.Info("Deleted Clavex client", "clientId", cr.Spec.ClientID, "org", orgID)
			controllerutil.RemoveFinalizer(&cr, clavexClientFinalizer)
			if err := r.Update(ctx, &cr); err != nil {
				return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
			}
		}
		return ctrl.Result{}, nil
	}

	// Ensure finalizer is present before we create anything remotely.
	if !controllerutil.ContainsFinalizer(&cr, clavexClientFinalizer) {
		controllerutil.AddFinalizer(&cr, clavexClientFinalizer)
		if err := r.Update(ctx, &cr); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		// Re-fetch after the metadata update to avoid acting on a stale
		// resourceVersion in the rest of this reconcile.
		if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
			return ctrl.Result{}, fmt.Errorf("re-fetching ClavexClient after finalizer update: %w", err)
		}
	}

	secret, drifted, err := r.reconcileClient(ctx, cvx, orgID, &cr)
	if err != nil {
		log.Error(err, "Failed to reconcile Clavex client", "clientId", cr.Spec.ClientID)
		r.setCondition(&cr, conditionTypeReady, metav1.ConditionFalse, "ReconcileError", err.Error())
		r.setCondition(&cr, conditionTypeSynced, metav1.ConditionFalse, "ReconcileError", err.Error())
		if statusErr := r.Status().Update(ctx, &cr); statusErr != nil {
			log.Error(statusErr, "Failed to update status after reconcile error")
		}
		return ctrl.Result{}, err
	}

	if secret != "" && cr.Spec.ClientSecretRef != nil {
		if err := r.writeClientSecret(ctx, &cr, secret); err != nil {
			log.Error(err, "Failed to persist client secret to Secret")
			return ctrl.Result{}, err
		}
	}

	syncReason, syncMessage := syncReasonSynced, "Client is in sync with the Admin API"
	if drifted {
		syncReason, syncMessage = syncReasonDriftCorrected, "Live client had diverged from spec out-of-band; corrected to match spec"
	}

	now := metav1.Now()
	cr.Status.ObservedGeneration = cr.Generation
	cr.Status.ClavexClientID = cr.Spec.ClientID
	cr.Status.LastSyncedAt = &now
	r.setCondition(&cr, conditionTypeReady, metav1.ConditionTrue, syncReasonSynced, "Client is in sync with the Admin API")
	r.setCondition(&cr, conditionTypeSynced, metav1.ConditionTrue, syncReason, syncMessage)
	if err := r.Status().Update(ctx, &cr); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	log.Info("Reconciled Clavex client", "clientId", cr.Spec.ClientID, "org", orgID)
	return ctrl.Result{RequeueAfter: requeueInterval}, nil
}

// reconcileClient creates or updates the remote OIDC client to match spec,
// returning the plaintext client secret if one was just (re)generated by a
// Create call (empty string otherwise — Update never returns a new secret),
// and whether the correction was due to out-of-band drift (as opposed to a
// legitimate spec change bumping .metadata.generation).
func (r *ClavexClientReconciler) reconcileClient(ctx context.Context, cvx *clavex.Client, orgID string, cr *clavexv1alpha1.ClavexClient) (secret string, drifted bool, err error) {
	existing, err := cvx.Clients.Get(ctx, orgID, cr.Spec.ClientID)
	if err != nil && !clavex.IsNotFound(err) {
		return "", false, fmt.Errorf("fetching existing client: %w", err)
	}
	// Clients.Get always returns a non-nil *OIDCClient, even on error (the
	// zero-value struct backing it), so pointer-nilness cannot be used to
	// detect "not found" — check the error instead.
	if err != nil {
		existing = nil
	}

	if existing == nil {
		isActive := cr.Spec.IsActive
		created, err := cvx.Clients.Create(ctx, orgID, clavex.CreateClientParams{
			Name:                cr.Spec.Name,
			ClientID:            cr.Spec.ClientID,
			RedirectURIs:        cr.Spec.RedirectURIs,
			PostLogoutRedirects: cr.Spec.PostLogoutRedirectURIs,
			GrantTypes:          cr.Spec.GrantTypes,
			ResponseTypes:       cr.Spec.ResponseTypes,
			IsPublic:            cr.Spec.IsPublic,
			IsActive:            &isActive,
		})
		if err != nil {
			return "", false, fmt.Errorf("creating client: %w", err)
		}
		clientSecret := ""
		if created.ClientSecret != nil {
			clientSecret = *created.ClientSecret
		}
		return clientSecret, false, nil
	}

	if clientUpToDate(existing, cr) {
		return "", false, nil
	}

	// The live client differs from spec. If .metadata.generation still
	// matches the last generation we observed, spec itself hasn't changed
	// since our last successful reconcile — the live resource must have
	// been edited out-of-band (clavexctl, Admin UI, direct API call). Spec
	// stays the source of truth, but we surface the correction via an
	// Event + a dedicated condition reason instead of a silent overwrite.
	drifted = cr.Status.ObservedGeneration == cr.Generation
	if drifted && r.Recorder != nil {
		r.Recorder.Eventf(cr, corev1.EventTypeNormal, eventReasonDriftDetected,
			"Live client %q no longer matches spec though it was not edited via kubectl; correcting via PATCH", cr.Spec.ClientID)
	}

	isActive := cr.Spec.IsActive
	_, err = cvx.Clients.Update(ctx, orgID, cr.Spec.ClientID, clavex.UpdateClientParams{
		Name:                &cr.Spec.Name,
		RedirectURIs:        cr.Spec.RedirectURIs,
		PostLogoutRedirects: cr.Spec.PostLogoutRedirectURIs,
		GrantTypes:          cr.Spec.GrantTypes,
		ResponseTypes:       cr.Spec.ResponseTypes,
		IsActive:            &isActive,
	})
	if err != nil {
		return "", drifted, fmt.Errorf("updating client: %w", err)
	}
	return "", drifted, nil
}

// clientUpToDate reports whether the live client already matches the
// mutable fields of spec, avoiding a no-op PATCH on every reconcile.
func clientUpToDate(live *clavex.OIDCClient, cr *clavexv1alpha1.ClavexClient) bool {
	return live.Name == cr.Spec.Name &&
		reflect.DeepEqual(live.RedirectURIs, cr.Spec.RedirectURIs) &&
		reflect.DeepEqual(live.PostLogoutRedirectURIs, cr.Spec.PostLogoutRedirectURIs) &&
		reflect.DeepEqual(live.GrantTypes, cr.Spec.GrantTypes) &&
		live.IsActive == cr.Spec.IsActive
}

// writeClientSecret persists a freshly generated client secret into the
// Secret referenced by spec.clientSecretRef, creating it if necessary.
func (r *ClavexClientReconciler) writeClientSecret(ctx context.Context, cr *clavexv1alpha1.ClavexClient, secretValue string) error {
	ref := cr.Spec.ClientSecretRef
	ns := ref.Namespace
	if ns == "" {
		ns = cr.Namespace
	}
	keyName := ref.Key
	if keyName == "" {
		keyName = authsecret.DefaultClientSecretKey
	}

	var secret corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ns}, &secret)
	if apierrors.IsNotFound(err) {
		secret = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: ref.Name, Namespace: ns},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{keyName: []byte(secretValue)},
		}
		return r.Create(ctx, &secret)
	}
	if err != nil {
		return fmt.Errorf("fetching clientSecretRef %s/%s: %w", ns, ref.Name, err)
	}
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[keyName] = []byte(secretValue)
	return r.Update(ctx, &secret)
}

// setCondition upserts a status condition, following standard K8s
// conventions (only bump LastTransitionTime when Status actually changes).
func (r *ClavexClientReconciler) setCondition(cr *clavexv1alpha1.ClavexClient, condType string, status metav1.ConditionStatus, reason, message string) {
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
func (r *ClavexClientReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&clavexv1alpha1.ClavexClient{}).
		Named("clavexclient").
		Complete(r)
}
