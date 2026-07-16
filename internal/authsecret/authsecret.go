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

// Package authsecret centralises how every CRD's reconciler (and the
// admission webhooks) resolve the Secret referenced by authSecretRef /
// clientSecretRef / signingKeyRef. It is a standalone package (rather than
// living in internal/controller) so that internal/webhook can validate a
// SecretRef at admission time without depending on the controller package.
package authsecret

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clavexv1alpha1 "github.com/clavex-eu/clavex-operator/api/v1alpha1"
)

const (
	// DefaultAPIKeySecretKey is the default data key holding the
	// org-scoped API key inside an authSecretRef Secret.
	DefaultAPIKeySecretKey = "apiKey"
	// OrgIDSecretKey is the fixed data key holding the org UUID inside an
	// authSecretRef Secret (never overridable via SecretRef.Key, unlike
	// the API key itself).
	OrgIDSecretKey = "orgId"
	// DefaultClientSecretKey is the default data key used for
	// ClientSecretRef/SigningKeyRef-style Secrets.
	DefaultClientSecretKey = "clientSecret"
)

// ResolveAuthSecret reads a SecretRef pointing at the operator's Model A
// auth Secret and returns the raw org-scoped API key and the org UUID to
// use as the {org_id} path segment. Shared by every CRD's reconciler
// (ClavexClient, ClavexIdentityProvider, …) since they all authenticate the
// same way — see ClavexClientSpec.AuthSecretRef for the Secret shape.
func ResolveAuthSecret(ctx context.Context, c client.Client, ref clavexv1alpha1.SecretRef, defaultNamespace string) (apiKey, orgID string, err error) {
	ns := ref.Namespace
	if ns == "" {
		ns = defaultNamespace
	}
	var secret corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ns}, &secret); err != nil {
		return "", "", fmt.Errorf("fetching authSecretRef %s/%s: %w", ns, ref.Name, err)
	}

	keyName := ref.Key
	if keyName == "" {
		keyName = DefaultAPIKeySecretKey
	}
	rawKey, ok := secret.Data[keyName]
	if !ok || len(rawKey) == 0 {
		return "", "", fmt.Errorf("authSecretRef %s/%s missing key %q", ns, ref.Name, keyName)
	}
	rawOrgID, ok := secret.Data[OrgIDSecretKey]
	if !ok || len(rawOrgID) == 0 {
		return "", "", fmt.Errorf("authSecretRef %s/%s missing key %q", ns, ref.Name, OrgIDSecretKey)
	}
	return string(rawKey), string(rawOrgID), nil
}

// ResolveSecretValue reads a single value out of an arbitrary Secret
// reference (e.g. ClientSecretRef), defaulting the data key and namespace.
func ResolveSecretValue(ctx context.Context, c client.Client, ref *clavexv1alpha1.SecretRef, defaultNamespace, fallbackKey string) (string, error) {
	if ref == nil {
		return "", nil
	}
	ns := ref.Namespace
	if ns == "" {
		ns = defaultNamespace
	}
	keyName := ref.Key
	if keyName == "" {
		keyName = fallbackKey
	}
	var secret corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ns}, &secret); err != nil {
		return "", fmt.Errorf("fetching secretRef %s/%s: %w", ns, ref.Name, err)
	}
	raw, ok := secret.Data[keyName]
	if !ok {
		return "", fmt.Errorf("secretRef %s/%s missing key %q", ns, ref.Name, keyName)
	}
	return string(raw), nil
}

// ValidateAuthSecretRef checks that the Secret referenced by ref exists in
// the resolved namespace and contains the expected keys, WITHOUT returning
// the secret values. Intended for admission-time validation (webhooks),
// where the goal is failing fast on a typo'd/missing Secret before the
// reconciler ever attempts a sync — not resolving credentials.
func ValidateAuthSecretRef(ctx context.Context, c client.Client, ref clavexv1alpha1.SecretRef, defaultNamespace string) error {
	_, _, err := ResolveAuthSecret(ctx, c, ref, defaultNamespace)
	return err
}
