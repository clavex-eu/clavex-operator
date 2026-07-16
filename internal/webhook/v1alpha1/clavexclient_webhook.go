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

package v1alpha1

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	clavexv1alpha1 "github.com/clavex-eu/clavex-operator/api/v1alpha1"
	"github.com/clavex-eu/clavex-operator/internal/authsecret"
)

// nolint:unused
// log is for logging in this package.
var clavexclientlog = logf.Log.WithName("clavexclient-resource")

// SetupClavexClientWebhookWithManager registers the webhook for ClavexClient in the manager.
func SetupClavexClientWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &clavexv1alpha1.ClavexClient{}).
		WithValidator(&ClavexClientCustomValidator{Client: mgr.GetClient()}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-clavex-clavex-eu-v1alpha1-clavexclient,mutating=false,failurePolicy=fail,sideEffects=None,groups=clavex.clavex.eu,resources=clavexclients,verbs=create;update,versions=v1alpha1,name=vclavexclient-v1alpha1.kb.io,admissionReviewVersions=v1

// ClavexClientCustomValidator validates ClavexClient on create/update. Its
// checks are deliberately limited to things CEL (+kubebuilder:validation:
// XValidation, already used in ClavexClientSpec for the public-client rule
// and clientId immutability) cannot express: reads of *other* cluster
// resources. Everything expressible in CRD schema/CEL stays there instead —
// it's cheaper (no extra network hop, works with kubectl --dry-run=server
// and offline validators) and doesn't need cert-manager.
type ClavexClientCustomValidator struct {
	// Client is used to look up the Secret referenced by
	// spec.authSecretRef so a typo'd/missing Secret is rejected at admission
	// time instead of surfacing only as a reconcile error later.
	Client client.Client
}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type ClavexClient.
func (v *ClavexClientCustomValidator) ValidateCreate(ctx context.Context, obj *clavexv1alpha1.ClavexClient) (admission.Warnings, error) {
	clavexclientlog.Info("Validation for ClavexClient upon creation", "name", obj.GetName())
	return nil, v.validateAuthSecretRef(ctx, obj)
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type ClavexClient.
func (v *ClavexClientCustomValidator) ValidateUpdate(ctx context.Context, _, newObj *clavexv1alpha1.ClavexClient) (admission.Warnings, error) {
	clavexclientlog.Info("Validation for ClavexClient upon update", "name", newObj.GetName())
	return nil, v.validateAuthSecretRef(ctx, newObj)
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type ClavexClient.
func (v *ClavexClientCustomValidator) ValidateDelete(_ context.Context, obj *clavexv1alpha1.ClavexClient) (admission.Warnings, error) {
	clavexclientlog.Info("Validation for ClavexClient upon deletion", "name", obj.GetName())
	// Deletion always succeeds validation: the controller's finalizer (not
	// this webhook) is responsible for a clean remote delete, and blocking
	// deletion here would make a CR permanently stuck if its Secret was
	// already removed.
	return nil, nil
}

// validateAuthSecretRef rejects the request if spec.authSecretRef does not
// resolve to an existing Secret with the required keys. Errors are wrapped
// with guidance rather than the raw client.Get error, since this message is
// what a user sees directly from `kubectl apply`.
func (v *ClavexClientCustomValidator) validateAuthSecretRef(ctx context.Context, obj *clavexv1alpha1.ClavexClient) error {
	if err := authsecret.ValidateAuthSecretRef(ctx, v.Client, obj.Spec.AuthSecretRef, obj.Namespace); err != nil {
		return fmt.Errorf("spec.authSecretRef is invalid: %w (create the Secret with apiKey/orgId keys before applying this ClavexClient)", err)
	}
	return nil
}
