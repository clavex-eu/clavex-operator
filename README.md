# clavex-operator

A native Kubernetes Operator for [Clavex](https://github.com/clavex-eu/clavex):
manage OIDC/SAML clients, identity providers, roles, groups, webhooks,
org-level security policies and auth-policy rules declaratively with
`kubectl apply`, instead of the imperative `clavexctl org apply` CLI
workflow. Reconciliation is continuous (drift between the CR and the live
Admin API state is detected and corrected automatically, not just at
apply time).

## Description

`clavexctl` already ships a Terraform-style IaC workflow
(`cmd/clavexctl/iac.go`: export/diff/plan/apply against the Clavex Admin
API v2). This operator extends the same declarative model to Kubernetes:
each Clavex resource type is a CRD in the `clavex.clavex.eu/v1alpha1`
group, reconciled by a single cluster-wide controller-manager against the
Admin API using an org-scoped API key (see `spec.authSecretRef` on every
CRD — the controller never holds cross-org/superadmin credentials of its
own).

Available CRDs: `ClavexClient` (OIDC client), `ClavexIdentityProvider`
(OIDC/SAML upstream IdP), `ClavexRole`, `ClavexGroup`,
`ClavexWebhook`, `ClavexOrg` (password policy / rate limits),
`ClavexAuthPolicy` (conditional access rules).

See [`docs/operator.md`](docs/operator.md) for a full walkthrough with
`kubectl apply` examples for every CRD, and the design rationale in the
package-level doc comments under `api/v1alpha1/`.

## Getting Started

### Prerequisites
- go version v1.24.6+
- docker version 17.03+.
- kubectl version v1.11.3+.
- Access to a Kubernetes v1.11.3+ cluster.
- [cert-manager](https://cert-manager.io/) installed in the target cluster
  (**required**, not optional): the base manifest ships a validating
  admission webhook for `ClavexClient` (checks `authSecretRef` at admission
  time), and cert-manager issues/rotates its serving certificate as well as
  the metrics endpoint's TLS certificate. `make deploy`/`kubectl apply -k
  config/default` will fail (`no matches for kind "Certificate"`) without it.

### Configure the Clavex Admin API URL
The manager requires `CLAVEX_SERVER_URL` at startup (fails fast if unset).
The base manifest ships a `manager-config` ConfigMap with an intentionally
invalid placeholder so a forgotten override is caught immediately rather
than silently pointing at the wrong Admin API. Override it in your own
Kustomize overlay before deploying:

```yaml
# my-overlay/kustomization.yaml
resources:
  - github.com/clavex-eu/clavex-operator/config/default
configMapGenerator:
  - name: manager-config
    behavior: merge
    literals:
      - clavex-server-url=https://admin.acme.example
```

```sh
kubectl apply -k my-overlay/
```

### Optional: Prometheus metrics via ServiceMonitor
`config/prometheus/` provides a `ServiceMonitor` scraping the manager's
TLS-secured metrics endpoint (cert-manager issued). It is **not** included
in `config/default` because it hard-depends on the Prometheus Operator CRDs
being installed in the cluster — enabling it on a cluster without Prometheus
Operator breaks `kubectl apply -k config/default` (`no matches for kind
"ServiceMonitor"`). If your cluster runs Prometheus Operator, add it via
your own overlay:

```yaml
# my-overlay/kustomization.yaml
resources:
  - github.com/clavex-eu/clavex-operator/config/default
  - github.com/clavex-eu/clavex-operator/config/prometheus
```

### To Deploy on the cluster
**Build and push your image to the location specified by `IMG`:**

```sh
make docker-build docker-push IMG=<some-registry>/clavex-operator:tag
```

**NOTE:** This image ought to be published in the personal registry you specified.
And it is required to have access to pull the image from the working environment.
Make sure you have the proper permission to the registry if the above commands don’t work.

**Install the CRDs into the cluster:**

```sh
make install
```

**Deploy the Manager to the cluster with the image specified by `IMG`:**

```sh
make deploy IMG=<some-registry>/clavex-operator:tag
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin
privileges or be logged in as admin.

**Create instances of your solution**
You can apply the samples (examples) from the config/sample:

```sh
kubectl apply -k config/samples/
```

>**NOTE**: Ensure that the samples has default values to test it out.

### To Uninstall
**Delete the instances (CRs) from the cluster:**

```sh
kubectl delete -k config/samples/
```

**Delete the APIs(CRDs) from the cluster:**

```sh
make uninstall
```

**UnDeploy the controller from the cluster:**

```sh
make undeploy
```

## Project Distribution

Following the options to release and provide this solution to the users.

### By providing a bundle with all YAML files

1. Build the installer for the image built and published in the registry:

```sh
make build-installer IMG=<some-registry>/clavex-operator:tag
```

**NOTE:** The makefile target mentioned above generates an 'install.yaml'
file in the dist directory. This file contains all the resources built
with Kustomize, which are necessary to install this project without its
dependencies.

2. Using the installer

Users can just run 'kubectl apply -f <URL for YAML BUNDLE>' to install
the project, i.e.:

```sh
kubectl apply -f https://raw.githubusercontent.com/<org>/clavex-operator/<tag or branch>/dist/install.yaml
```

### By providing a Helm Chart

1. Build the chart using the optional helm plugin

```sh
kubebuilder edit --plugins=helm/v2-alpha
```

2. See that a chart was generated under 'dist/chart', and users
can obtain this solution from there.

**NOTE:** If you change the project, you need to update the Helm Chart
using the same command above to sync the latest changes. Furthermore,
if you create webhooks, you need to use the above command with
the '--force' flag and manually ensure that any custom configuration
previously added to 'dist/chart/values.yaml' or 'dist/chart/manager/manager.yaml'
is manually re-applied afterwards.

## Contributing
// TODO(user): Add detailed information on how you would like others to contribute to this project

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

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

