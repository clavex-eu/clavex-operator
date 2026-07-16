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
	"maps"
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
)

const (
	// clavexIDPFinalizer ensures the remote identity provider is deleted
	// from the Admin API before the CR is removed from the cluster.
	clavexIDPFinalizer = "clavex.eu/clavexidentityprovider-finalizer"

	// eventReasonIDPDriftDetected mirrors eventReasonDriftDetected for
	// ClavexClient — see reconcileIDP for when it fires.
	eventReasonIDPDriftDetected = "DriftDetected"
)

// ClavexIdentityProviderReconciler reconciles a ClavexIdentityProvider object
type ClavexIdentityProviderReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Recorder emits Kubernetes Events — see ClavexClientReconciler.Recorder.
	Recorder record.EventRecorder

	// ClavexServerURL is the base URL of the Clavex Admin API. See
	// ClavexClientReconciler.ClavexServerURL.
	ClavexServerURL string
}

// +kubebuilder:rbac:groups=clavex.clavex.eu,resources=clavexidentityproviders,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=clavex.clavex.eu,resources=clavexidentityproviders/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=clavex.clavex.eu,resources=clavexidentityproviders/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile moves the live Clavex identity provider closer to the desired
// state described by a ClavexIdentityProvider CR, using an org-scoped Admin
// API key read from spec.authSecretRef. Unlike ClavexClient, there is no
// client-supplied stable ID: existing providers are looked up by spec.name.
func (r *ClavexIdentityProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var cr clavexv1alpha1.ClavexIdentityProvider
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting ClavexIdentityProvider: %w", err)
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

	// Handle deletion.
	if !cr.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&cr, clavexIDPFinalizer) {
			existing, findErr := findIDPByName(ctx, cvx, orgID, cr.Spec.Name)
			if findErr != nil {
				log.Error(findErr, "Failed to look up identity provider before delete", "name", cr.Spec.Name)
				return ctrl.Result{}, findErr
			}
			if existing != nil {
				if err := cvx.IdentityProviders.Delete(ctx, orgID, existing.ID); err != nil && !clavex.IsNotFound(err) {
					log.Error(err, "Failed to delete Clavex identity provider", "name", cr.Spec.Name)
					return ctrl.Result{}, err
				}
				log.Info("Deleted Clavex identity provider", "name", cr.Spec.Name, "org", orgID)
			}
			controllerutil.RemoveFinalizer(&cr, clavexIDPFinalizer)
			if err := r.Update(ctx, &cr); err != nil {
				return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
			}
		}
		return ctrl.Result{}, nil
	}

	// Ensure finalizer is present before we create anything remotely.
	if !controllerutil.ContainsFinalizer(&cr, clavexIDPFinalizer) {
		controllerutil.AddFinalizer(&cr, clavexIDPFinalizer)
		if err := r.Update(ctx, &cr); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
			return ctrl.Result{}, fmt.Errorf("re-fetching ClavexIdentityProvider after finalizer update: %w", err)
		}
	}

	idpID, drifted, err := r.reconcileIDP(ctx, cvx, orgID, &cr)
	if err != nil {
		log.Error(err, "Failed to reconcile Clavex identity provider", "name", cr.Spec.Name)
		r.setCondition(&cr, conditionTypeReady, metav1.ConditionFalse, "ReconcileError", err.Error())
		r.setCondition(&cr, conditionTypeSynced, metav1.ConditionFalse, "ReconcileError", err.Error())
		if statusErr := r.Status().Update(ctx, &cr); statusErr != nil {
			log.Error(statusErr, "Failed to update status after reconcile error")
		}
		return ctrl.Result{}, err
	}

	syncReason, syncMessage := syncReasonSynced, "Identity provider is in sync with the Admin API"
	if drifted {
		syncReason, syncMessage = syncReasonDriftCorrected, "Live identity provider had diverged from spec out-of-band; corrected to match spec"
	}

	now := metav1.Now()
	cr.Status.ObservedGeneration = cr.Generation
	cr.Status.ClavexIDPID = idpID
	cr.Status.LastSyncedAt = &now
	r.setCondition(&cr, conditionTypeReady, metav1.ConditionTrue, syncReasonSynced, "Identity provider is in sync with the Admin API")
	r.setCondition(&cr, conditionTypeSynced, metav1.ConditionTrue, syncReason, syncMessage)
	if err := r.Status().Update(ctx, &cr); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	log.Info("Reconciled Clavex identity provider", "name", cr.Spec.Name, "org", orgID)
	return ctrl.Result{RequeueAfter: requeueInterval}, nil
}

// findIDPByName lists identity providers in orgID and returns the one whose
// Name matches, or nil if none does. This is the only way to reconcile by a
// stable key since the Admin API assigns its own opaque ID on creation.
func findIDPByName(ctx context.Context, cvx *clavex.Client, orgID, name string) (*clavex.IdentityProvider, error) {
	idps, err := cvx.IdentityProviders.List(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("listing identity providers: %w", err)
	}
	for i := range idps {
		if idps[i].Name == name {
			return &idps[i], nil
		}
	}
	return nil, nil
}

// buildIDPParams resolves spec + the optional clientSecretRef into the
// CreateIDPParams shape shared by Create and Update.
func (r *ClavexIdentityProviderReconciler) buildIDPParams(ctx context.Context, cr *clavexv1alpha1.ClavexIdentityProvider) (clavex.CreateIDPParams, error) {
	enabled := cr.Spec.Enabled
	config := maps.Clone(cr.Spec.Config)
	if config == nil {
		config = map[string]string{}
	}
	if cr.Spec.ClientID != "" {
		config["client_id"] = cr.Spec.ClientID
	}
	if cr.Spec.ClientSecretRef != nil {
		secretValue, err := authsecret.ResolveSecretValue(ctx, r.Client, cr.Spec.ClientSecretRef, cr.Namespace, authsecret.DefaultClientSecretKey)
		if err != nil {
			return clavex.CreateIDPParams{}, fmt.Errorf("resolving clientSecretRef: %w", err)
		}
		config["client_secret"] = secretValue
	}

	return clavex.CreateIDPParams{
		Name:              cr.Spec.Name,
		Type:              cr.Spec.ProviderType,
		Enabled:           &enabled,
		Config:            config,
		AllowJIT:          cr.Spec.AllowJIT,
		RolesClaim:        cr.Spec.RolesClaim,
		RoleClaimMappings: cr.Spec.RoleClaimMappings,
	}, nil
}

// reconcileIDP creates or updates the remote identity provider to match
// spec, returning its Admin API ID and whether the update (if any) was due
// to out-of-band drift rather than a legitimate spec change.
func (r *ClavexIdentityProviderReconciler) reconcileIDP(ctx context.Context, cvx *clavex.Client, orgID string, cr *clavexv1alpha1.ClavexIdentityProvider) (idpID string, drifted bool, err error) {
	existing, err := findIDPByName(ctx, cvx, orgID, cr.Spec.Name)
	if err != nil {
		return "", false, err
	}

	params, err := r.buildIDPParams(ctx, cr)
	if err != nil {
		return "", false, err
	}

	if existing == nil {
		created, err := cvx.IdentityProviders.Create(ctx, orgID, params)
		if err != nil {
			return "", false, fmt.Errorf("creating identity provider: %w", err)
		}
		return created.ID, false, nil
	}

	if idpUpToDate(existing, cr) {
		return existing.ID, false, nil
	}

	drifted = cr.Status.ObservedGeneration == cr.Generation
	if drifted && r.Recorder != nil {
		r.Recorder.Eventf(cr, corev1.EventTypeNormal, eventReasonIDPDriftDetected,
			"Live identity provider %q no longer matches spec though it was not edited via kubectl; correcting via PATCH", cr.Spec.Name)
	}

	if _, err := cvx.IdentityProviders.Update(ctx, orgID, existing.ID, params); err != nil {
		return "", drifted, fmt.Errorf("updating identity provider: %w", err)
	}
	return existing.ID, drifted, nil
}

// idpUpToDate reports whether the live identity provider already matches
// the mutable fields of spec, avoiding a no-op PATCH on every reconcile.
// Config is compared shallowly: client_secret is intentionally excluded
// since the Admin API never echoes it back in Get/List responses, so any
// comparison against it would always look like drift.
func idpUpToDate(live *clavex.IdentityProvider, cr *clavexv1alpha1.ClavexIdentityProvider) bool {
	if live.Name != cr.Spec.Name ||
		live.ProviderType != cr.Spec.ProviderType ||
		live.IsActive != cr.Spec.Enabled ||
		live.AllowJIT != cr.Spec.AllowJIT ||
		live.ClientID != cr.Spec.ClientID {
		return false
	}
	if cr.Spec.RolesClaim != "" {
		if live.RolesClaim == nil || *live.RolesClaim != cr.Spec.RolesClaim {
			return false
		}
	}
	return reflect.DeepEqual(live.RoleClaimMappings, cr.Spec.RoleClaimMappings)
}

// setCondition upserts a status condition on a ClavexIdentityProvider —
// see ClavexClientReconciler.setCondition for the shared convention.
func (r *ClavexIdentityProviderReconciler) setCondition(cr *clavexv1alpha1.ClavexIdentityProvider, condType string, status metav1.ConditionStatus, reason, message string) {
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
func (r *ClavexIdentityProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&clavexv1alpha1.ClavexIdentityProvider{}).
		Named("clavexidentityprovider").
		Complete(r)
}
