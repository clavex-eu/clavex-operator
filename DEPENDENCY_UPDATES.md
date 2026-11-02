# Dependency maintenance — 2026-11-02

Updated compatible dependencies while keeping Kubernetes libraries on the 0.36 patch line (0.36.6) and controller-runtime on 0.24.1. Updated CEL to 0.30.0 for security fixes, and the existing gRPC, OpenTelemetry and HTTP dependencies. Go module and Docker builder now use Go 1.26.10. Updated clavex-sdk-go to v0.4.0 after checking the additive DevicePKI tag diff.

Validation uses isolated envtest API server/etcd binaries (Kubernetes 1.36.2), the Go race detector, go vet, go mod verify and govulncheck. No generated CRDs, resource schemas, deployed controller or real cluster configuration were modified. Full deployment E2E/upgrade tests against a dedicated Kind cluster were not run locally.

Pinned the custom logcheck linter plugin to logtools v0.11.0 instead of a floating latest version for reproducible lint builds.

The repository's custom `make lint` (golangci-lint 2.12.2 with logcheck 0.11.0) completed but reported 8 issues in existing source: forbidden sort import; two unchecked WebSocket Close calls; repeated test string; legacy atomic counter; and three deprecated GetEventRecorderFor calls. These are not suppressed. Keep this maintenance PR in draft until the source/lint follow-up is addressed.
