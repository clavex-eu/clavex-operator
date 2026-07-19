# Clavex Kubernetes Operator — usage guide

This guide walks through installing the operator and managing every Clavex
resource type declaratively with `kubectl apply`. It assumes you already
have a Clavex Admin API v2 deployment reachable from the cluster.

## 1. Install the operator

```sh
# Latest stable release (consolidated manifest: CRDs + RBAC + Deployment):
kubectl apply -f https://github.com/clavex-eu/clavex-operator/releases/latest/download/install.yaml

# Or pin an exact version:
kubectl apply -f https://github.com/clavex-eu/clavex-operator/releases/download/v0.1.0/install.yaml
```

Each `install.yaml` is attached to its GitHub Release with the
controller-manager image pinned to that release's version
(`ghcr.io/clavex-eu/clavex-operator:<tag>`). It is published per tag and is
**not** committed to any branch, so pinning a version gives a reproducible
install instead of whatever `main` happened to build.

Before applying, you **must** point the manager at your Admin API — see
"Configure the Clavex Admin API URL" in [`README.md`](../README.md). If
you build `dist/install.yaml` yourself via `make build-installer`, edit
the `manager-config` ConfigMap's `clavex-server-url` key (or apply a
Kustomize overlay with `configMapGenerator` + `behavior: merge`, which is
the recommended approach and survives re-running `make build-installer`).

The controller-manager is a single, cluster-wide Deployment: one
controller-manager watches CRs across all namespaces, so a single
install serves every team/namespace in the cluster. RBAC is granted via
`ClusterRole`/`ClusterRoleBinding` (see `config/rbac/role.yaml`).

## 2. Mint an org-scoped API key

Every CRD authenticates to the Admin API via an org-scoped API key (see
`spec.authSecretRef` on each CR). The operator never holds superadmin or
cross-org credentials — reconciliation is entirely constrained to the
org the key belongs to (enforced by the Admin API's `RequireOrgAccess`
middleware).

A superadmin (today, minting org-scoped keys is a superadmin-only
operation — see the auth-model decision in the implementation plan) mints
one per org via the existing `POST /api/v1/superadmin/api-keys` endpoint,
passing `org_id`. Then create the Secret the CRs will reference:

```sh
kubectl create secret generic acme-admin-api-key \
  --from-literal=apiKey='<the minted API key>' \
  --from-literal=orgId='<the org UUID>'
```

Every `authSecretRef` in the samples below expects a Secret with exactly
these two keys (`apiKey`, `orgId`) in the CR's own namespace. To reference
a Secret in another namespace, set `authSecretRef.namespace` explicitly.

## 3. Apply resources

All samples below live in [`config/samples/`](../config/samples/) and can
be applied as-is (after creating the Secret above) or used as templates.
Apply everything at once with:

```sh
kubectl apply -k config/samples/
```

Or apply resources individually as shown per-CRD below.

### ClavexClient — OIDC client

```yaml
apiVersion: clavex.clavex.eu/v1alpha1
kind: ClavexClient
metadata:
  name: acme-dashboard
spec:
  orgRef: acme
  authSecretRef:
    name: acme-admin-api-key
  clientId: acme-dashboard
  name: Acme Dashboard
  redirectUris:
    - https://dashboard.acme.example/callback
  grantTypes:
    - authorization_code
    - refresh_token
  scopes: [openid, profile, email]
  isPublic: false
  tokenEndpointAuthMethod: client_secret_basic
  clientSecretRef:
    name: acme-dashboard-client-secret
```

`clientId` is the reconciliation key and is immutable after creation. The
controller **generates** the client secret on first create and writes it
to the Secret named in `clientSecretRef` — it is never read back from
that Secret afterwards.

```sh
kubectl apply -f config/samples/clavex_v1alpha1_clavexclient.yaml
kubectl get clavexclients
kubectl get secret acme-dashboard-client-secret -o jsonpath='{.data.clientSecret}' | base64 -d
```

### ClavexIdentityProvider — upstream OIDC/SAML IdP

```yaml
apiVersion: clavex.clavex.eu/v1alpha1
kind: ClavexIdentityProvider
metadata:
  name: acme-okta
spec:
  orgRef: acme
  authSecretRef:
    name: acme-admin-api-key
  name: Acme Corporate Okta
  providerType: oidc
  clientId: acme-okta-client
  clientSecretRef:
    name: acme-okta-client-secret
  config:
    authorizationUrl: https://acme.okta.com/oauth2/v1/authorize
    tokenUrl: https://acme.okta.com/oauth2/v1/token
    userinfoUrl: https://acme.okta.com/oauth2/v1/userinfo
  allowJit: true
  rolesClaim: groups
```

`name` is the reconciliation key here (the Admin API assigns its own
opaque IdP ID, so the controller looks providers up by name). **Renaming
`spec.name` creates a new IdP instead of renaming the existing one** —
the same limitation `clavexctl org apply` already has. Unlike
`ClavexClient`, `clientSecretRef` here is **user-supplied**: create the
Secret yourself before applying, the controller only reads it.

### ClavexRole

```yaml
apiVersion: clavex.clavex.eu/v1alpha1
kind: ClavexRole
metadata:
  name: acme-admin-role
spec:
  orgRef: acme
  authSecretRef:
    name: acme-admin-api-key
  name: admin
  description: Full administrative access to the Acme organisation.
```

Roles have no update endpoint in the Admin API: once created, editing
`description` in the CR has no effect on the live role (create/delete
only). Delete the CR to delete the role.

### ClavexGroup — role membership by name

```yaml
apiVersion: clavex.clavex.eu/v1alpha1
kind: ClavexGroup
metadata:
  name: acme-platform-team
spec:
  orgRef: acme
  authSecretRef:
    name: acme-admin-api-key
  name: platform-team
  roles:
    - admin
    - member
```

`roles` lists **role names**, not IDs. Apply order relative to the
`ClavexRole` resources referenced doesn't matter: if a name doesn't
resolve yet, the controller requeues (15s) instead of failing — it
becomes consistent once the corresponding `ClavexRole` is applied.

### ClavexWebhook

```yaml
apiVersion: clavex.clavex.eu/v1alpha1
kind: ClavexWebhook
metadata:
  name: acme-events-webhook
spec:
  orgRef: acme
  authSecretRef:
    name: acme-admin-api-key
  url: https://events.acme.example/webhooks/clavex
  events:
    - user.login
    - user.password.changed
  signingKeyRef:
    name: acme-webhook-signing-key
```

`url` is the reconciliation key (the Admin API's webhook model has no
`name` field). `signingKeyRef` is user-supplied, like the IdP client
secret — create that Secret first.

### ClavexOrg — password policy & rate limits

```yaml
apiVersion: clavex.clavex.eu/v1alpha1
kind: ClavexOrg
metadata:
  name: acme-org-settings
spec:
  orgRef: acme
  authSecretRef:
    name: acme-admin-api-key
  passwordPolicy:
    minLength: 12
    requireUpper: true
    requireLower: true
    requireNumber: true
    requireSpecial: true
    maxAgeDays: 90
    historyCount: 5
  rateLimits:
    maxAttemptsPerMinute: 10
    lockoutDurationSeconds: 900
```

Both `passwordPolicy` and `rateLimits` are optional and independent —
omit either to leave that section unmanaged (every org already has
defaults; there's nothing to bootstrap). Unlike every other CRD, deleting
a `ClavexOrg` CR has **no effect on the Admin API** (no finalizer): it
simply stops managing those settings, it does not reset them to defaults.
Org lifecycle (create/delete an org) is out of scope for this CRD — it
requires superadmin privileges incompatible with the operator's
org-scoped auth model, and remains a `clavexctl`/Admin-UI bootstrap step.

### ClavexAuthPolicy — conditional access rules

```yaml
apiVersion: clavex.clavex.eu/v1alpha1
kind: ClavexAuthPolicy
metadata:
  name: acme-require-mfa-off-hours
spec:
  orgRef: acme
  authSecretRef:
    name: acme-admin-api-key
  name: require-mfa-outside-office-hours
  priority: 50
  action: require_mfa
  conditions:
    notCountries: [IT]
    mfaEnrolled: false
    daysOfWeek: [Sat, Sun]
    hourRange:
      from: 20
      to: 6
```

`name` is the reconciliation key; unlike `ClavexRole`, this CRD supports
real updates — editing `priority`/`action`/`conditions` converges the
live rule via a PUT on every reconcile.

## 3b. Admission validation (ClavexClient)

`ClavexClient` has a validating admission webhook that checks
`spec.authSecretRef` resolves to an existing `Secret` with the required
`apiKey`/`orgId` keys **before** the CR is persisted — misconfigured
references are rejected at `kubectl apply` time instead of surfacing later
as a stuck `Synced: False` condition. This requires cert-manager in the
cluster (see the README prerequisites); every other CRD relies solely on
CEL validation rules embedded in the CRD schema (immutability, enums,
required fields), with no live-cluster admission check yet.

## 4. Observing drift and status

Every CRD exposes `status.conditions` (`Ready`, `Synced`) and
`status.observedGeneration`. When the controller detects the live Admin
API state has drifted from a CR out-of-band (e.g. edited via `clavexctl`
or the Admin UI, with no corresponding spec change), it emits a
`DriftDetected` Kubernetes Event and corrects the value back to what the
CR declares — the spec always remains the source of truth (GitOps-safe),
but the correction is visible instead of a silent overwrite:

```sh
kubectl describe clavexclient acme-dashboard
kubectl get events --field-selector reason=DriftDetected
```

## 5. Uninstalling a resource

Deleting a CR (except `ClavexOrg`, see above) removes the corresponding
object from the Admin API via a finalizer — `kubectl delete` blocks until
the remote delete succeeds, then the finalizer is removed and the object
disappears from `kubectl get`.

## 6. Security model — cluster-wide Secret access

**Read this before installing.** The controller-manager's `ClusterRole`
(`config/rbac/role.yaml`) grants cluster-wide read *and* write access to
**all `Secret` objects in every namespace**:

```yaml
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["get", "list", "watch", "create", "update", "patch"]
```

### Why it needs this
- **Read (`get`/`list`/`watch`)** — every CR authenticates to the Admin API
  through the Secret named in `spec.authSecretRef`, and that Secret may live
  in a namespace *other* than the CR's own (`spec.authSecretRef.namespace`).
  Because the reference can point at any namespace, the role is not scoped to
  a fixed set. The `ClavexClient` admission webhook resolves the same
  reference at admission time (section 3b).
- **Write (`create`/`update`/`patch`)** — `ClavexClient` **generates** the
  OIDC client secret on first create and writes it back to the Secret named
  in `spec.clientSecretRef` (section 3, ClavexClient), again potentially in
  any namespace.

### The trust implication (stated plainly)
A compromise of the controller-manager Pod — or of a principal able to
exec into it or read its ServiceAccount token — grants read/write access to
**every Secret in the cluster**, including Secrets that have nothing to do
with Clavex (database passwords, TLS keys, cloud credentials, other
operators' secrets). This is a broad blast radius. Treat the
controller-manager namespace as a high-privilege tier: restrict who can
create Pods, exec, or read ServiceAccount tokens there.

### Reducing the blast radius
- **Namespace-scoped mode is *not* currently available.** The manager has no
  `--watch-namespace` / `--namespace-scoped` flag today (`cmd/main.go` sets
  up a cluster-wide cache), and the RBAC ships as `ClusterRole` +
  `ClusterRoleBinding`. Narrowing it to specific namespaces would require a
  code change (constrain the manager cache to a namespace set) plus swapping
  the `ClusterRole`/`ClusterRoleBinding` for namespaced `Role`/`RoleBinding`
  objects. It is a candidate future option, not a supported toggle.
- **NetworkPolicy** — `config/network-policy/` ships policies that restrict
  ingress to the manager's metrics (`allow-metrics-traffic.yaml`) and webhook
  (`allow-webhook-traffic.yaml`) endpoints. They are **not** part of
  `config/default` by default; enable them via your own overlay:

  ```yaml
  resources:
    - github.com/clavex-eu/clavex-operator/config/default
    - github.com/clavex-eu/clavex-operator/config/network-policy
  ```

  Note these limit *network reach to* the manager; they do not narrow the
  Secret RBAC scope above.
- **Keep `authSecretRef`/`clientSecretRef` Secrets in the operator's own
  namespace** where practical: the RBAC grant is unavoidably cluster-wide,
  but co-locating the Secrets keeps normal operation off other teams'
  namespaces even though the *permission* to reach them remains.
