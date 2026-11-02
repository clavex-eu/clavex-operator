# Dependency maintenance — 2026-11-02

Updated compatible dependencies while keeping Kubernetes libraries on the 0.36 patch line (0.36.6) and controller-runtime on 0.24.1. Updated CEL to 0.30.0 for security fixes, and the existing gRPC, OpenTelemetry and HTTP dependencies. Go module and Docker builder now use Go 1.26.10. Updated clavex-sdk-go to v0.4.0 after checking the additive DevicePKI tag diff.

Validation uses isolated envtest API server/etcd binaries (Kubernetes 1.36.2), the Go race detector, go vet, go mod verify and govulncheck. No generated CRDs, resource schemas, deployed controller or real cluster configuration were modified. Full deployment E2E/upgrade tests against a dedicated Kind cluster were not run locally.

Pinned the custom logcheck linter plugin to logtools v0.11.0 instead of a floating latest version for reproducible lint builds.

Resolved the 8 lint issues without suppressions: use slices.Sort, explicitly discard WebSocket cleanup errors, use atomic.Int32 and a shared test constant, and migrate all seven controllers to GetEventRecorder and the events.EventRecorder interface. Drift notifications retain their reason and note, with an explicit Reconcile action. The drift-correction integration test now checks that initial creation emits no drift notification and a subsequent out-of-band change does.

Regenerated RBAC from controller markers to grant create/patch for events.k8s.io events, while preserving the core events permissions used by other manager components. An eventual deployment upgrade must apply this updated RBAC alongside the manager image. CRD schemas and generated Go API definitions are unchanged. No live cluster was changed.
