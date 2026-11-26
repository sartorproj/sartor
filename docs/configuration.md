# Configuration Reference

Complete reference for Sartor configuration options. Durations use Go syntax (`168h`, `30m`); day units such as `7d` are rejected.

## Controller

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--enable-git-writeback` | `false` | Open and update pull requests. When `false`, Sartor never reads Git credentials or calls the Git provider; recommendations only appear in Tailoring status. |
| `--analysis-interval` | `6h` | How often each Tailoring is re-analyzed. |
| `--enable-oom-patching` | `false` | Raise memory limits of OOMKilled containers. Only workloads with a non-paused Tailoring are touched. |
| `--leader-elect` | `false` | Enable leader election. |
| `--metrics-bind-address` | `0` | Metrics endpoint address (`:8443` for HTTPS, `:8080` for HTTP, `0` disables it). |
| `--metrics-secure` | `true` | Serve metrics over HTTPS. |
| `--health-probe-bind-address` | `:8081` | Health and readiness probe address. |

### Environment

| Variable | Description |
|----------|-------------|
| `POD_NAMESPACE` | Namespace the controller runs in (set it from `metadata.namespace` with the downward API). Secrets are only read from this namespace. Required when `--enable-git-writeback` is set. |

### Secrets

Every `secretRef` must point to the controller's namespace; leaving `namespace` empty means the controller's namespace. Secrets are read with a direct `get` (no list or watch), so the controller needs `get` on Secrets in its own namespace only.

## Atelier

The `Atelier` resource is cluster-scoped and defines global configuration. Sartor uses the first Atelier it finds; create exactly one.

### Spec Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `prometheus` | PrometheusConfig | Yes | Prometheus-compatible query API |
| `gitProvider` | GitProviderConfig | Yes | Git provider configuration |
| `safetyRails` | SafetyRailsConfig | No | Bounds for recommended values |
| `prSettings` | PRSettingsConfig | No | Pull request behavior settings |
| `argocd` | ArgoCDConfig | No | ArgoCD connectivity (sync is not implemented yet) |
| `opencost` | OpenCostConfig | No | Not used by the controller yet |
| `batchMode` | bool | No | Not implemented yet; every Tailoring gets its own pull request |
| `defaultAnalysisWindow` | Duration | No | Default time window for analysis (default: `168h`) |

### PrometheusConfig

```yaml
prometheus:
  url: "http://prometheus.monitoring.svc:9090"
  secretRef:            # optional
    name: prometheus-auth
    namespace: sartor-system
  insecureSkipVerify: false
```

The Secret holds `username` and `password` for basic auth, or `token` for a bearer token.

`url` is the base of a Prometheus-compatible API. Sartor appends `/api/v1/...` and keeps the path prefix and query string of the URL on every request:

| Backend | `url` |
|---------|-------|
| Prometheus | `http://prometheus.monitoring.svc:9090` |
| VictoriaMetrics single-node | `http://victoria-metrics.example.svc:8428` |
| VictoriaMetrics cluster (tenant 0) | `http://vmselect.example.svc:8481/select/0/prometheus` |
| VictoriaMetrics cluster, one cluster's series only | `http://vmselect.example.svc:8481/select/0/prometheus?extra_label=cluster=example-cluster` |

### GitProviderConfig

```yaml
gitProvider:
  type: github  # only github supports write-back today
  secretRef:
    name: github-token
    namespace: sartor-system
  baseURL: ""   # GitHub Enterprise Server, e.g. https://github.example.com
```

The Secret contains either a token, or GitHub App credentials:

| Key | Description |
|-----|-------------|
| `token` | Personal access token (fine-grained: Contents and Pull requests read/write) |
| `appId` | GitHub App ID |
| `installationId` | Installation ID of the App on your organization |
| `privateKey` | The App's PEM private key |

Set either `token` or all three App keys, not both.

### SafetyRailsConfig

```yaml
safetyRails:
  minCPU: "10m"
  minMemory: "64Mi"
  maxCPU: "8"
  maxMemory: "32Gi"
```

Rails apply to requests and limits after the `maxChangePercent` cap, so they always win. A FitProfile's `safetyRailsOverride` replaces these values for Tailorings that use it.

### PRSettingsConfig

```yaml
prSettings:
  cooldownPeriod: 168h  # minimum time between pull requests once the last one is merged or closed
  minChangePercent: 20  # smallest request change worth a pull request
  baseBranch: "main"    # used when writeBack.branch is empty
  branchPrefix: "sartor/"
  labels:
    - "automation"
    - "resource-optimization"
```

Sartor's branch for a Tailoring is `<branchPrefix><namespace>/<name>`. If several clusters write to the same repository, give each its own `branchPrefix` (for example `sartor/example-cluster/`) so their branches do not collide.

Closing a pull request with the `sartor-ignore` label sets `status.ignored` on the Tailoring; Sartor stops proposing changes for it. Remove the flag by editing the status or recreating the Tailoring.

### ArgoCDConfig

```yaml
argocd:
  enabled: true
  serverURL: "https://argocd.example.com"  # Optional
  secretRef:
    name: argocd-token
    namespace: sartor-system
  namespace: "argocd"
```

Only the connectivity check (`status.argoCDConnected`) is implemented. `syncOnMerge`, `refreshOnPRCreate` and `hardRefresh` are accepted but do nothing yet; rely on ArgoCD auto-sync instead.

## FitProfile

The `FitProfile` resource is namespace-scoped and defines the recommendation strategy.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `strategy` | string | Yes | `percentile` or `dsp` |
| `parameters` | object | No | Strategy parameters (see [Recommendation Strategies](recommendation-strategies.md)) |
| `safetyRailsOverride` | SafetyRailsConfig | No | Replaces the Atelier's safety rails |
| `displayName`, `description`, `priority`, `labels` | | No | Informational |

`parameters.maxChangePercent` (default `50`) caps how far a single pull request may move any value from what the workload runs today. `100` or more disables the cap.

## Tailoring

The `Tailoring` resource is namespace-scoped and defines per-workload optimization.

### Spec Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `target` | TargetRef | Yes | Workload to optimize |
| `fitProfileRef` | FitProfileRef | Yes | FitProfile to use (`name`, optional `namespace`, default the Tailoring's) |
| `writeBack` | WriteBackConfig | Yes | Where recommendations are written |
| `analysisWindow` | Duration | No | Time window for metrics (default from Atelier, `168h`) |
| `paused` | bool | No | Keep analyzing but never touch Git |
| `excludeContainers` | []string | No | Containers to leave alone (for example injected sidecars) |

### TargetRef

```yaml
target:
  kind: Deployment  # Deployment, StatefulSet, DaemonSet
  name: my-app
```

### WriteBackConfig

```yaml
writeBack:
  repository: github.com/org/manifests  # or org/manifests, or a clone URL
  path: apps/my-app/values.yaml         # relative to the repository root
  type: helm                            # raw, helm or kustomize
  helmValuePath: app.resources          # helm only
  branch: main                          # optional, default prSettings.baseBranch
```

| `type` | File | Behavior |
|--------|------|----------|
| `raw` | Kubernetes manifest, multi-document files allowed | Updates `resources` of the containers in the document whose kind and `metadata.name` match `target`. |
| `helm` | Helm values file | Updates the map at `helmValuePath`, which must already exist. With several containers, map each one: `helmValuePath: "app=app.resources,worker=worker.resources"`. |
| `kustomize` | Strategic merge patch | Updates container resources in the patch. A missing patch is created and registered in the `kustomization.yaml` of the same directory. |

Comments, key order, quoting and indentation are preserved. Limits are only written for containers that already have them.

## Status Fields

### AtelierStatus

- `prometheusConnected`: Connection status
- `gitProviderConnected`: Connection status (`false` with reason `WriteBackDisabled` when write-back is off)
- `argoCDConnected`: Connection status (if enabled)
- `managedTailorings`: Count of active Tailorings
- `conditions`: Kubernetes conditions

### TailoringStatus

- `targetFound`: Whether target workload exists
- `lastAnalysis`: Latest analysis results and recommendations
- `prState`, `prUrl`, `prNumber`: Sartor's pull request for this Tailoring
- `branchName`, `commitSha`, `lastPRUpdateTime`: Last write to the branch
- `mergedAt`: When the pull request was merged
- `ignored`: Whether a pull request was closed with the `sartor-ignore` label
- `conditions`: `Ready`, `PRCreated`, `PRMerged`, and others. `PRCreated` explains why no pull request was opened (`WriteBackDisabled`, `InvalidWriteBack`, `GitCredentialsError`, ...).

## Examples

See [config/samples](../config/samples) for complete examples.
