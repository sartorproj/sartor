# Configuration Reference

Complete reference for Sartor configuration options.

## Atelier

The `Atelier` resource is cluster-scoped and defines global configuration.

### Spec Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `prometheus` | PrometheusConfig | Yes | Prometheus connection settings |
| `gitProvider` | GitProviderConfig | Yes | Git provider configuration |
| `safetyRails` | SafetyRailsConfig | No | Resource limits and constraints |
| `prSettings` | PRSettingsConfig | No | Pull request behavior settings |
| `argocd` | ArgoCDConfig | No | ArgoCD integration settings |
| `batchMode` | bool | No | Batch multiple changes into single PR |
| `defaultAnalysisWindow` | Duration | No | Default time window for analysis (default: 168h) |

### PrometheusConfig

```yaml
prometheus:
  url: "http://prometheus:9090"
  secretRef:
    name: prometheus-auth
    namespace: sartor-system
  insecureSkipVerify: false
  clusterLabel: cluster  # Optional: label holding the cluster name when one backend stores several clusters
```

### GitProviderConfig

```yaml
gitProvider:
  type: github  # github, gitlab, bitbucket
  secretRef:
    name: github-token
    namespace: sartor-system
  baseURL: ""  # For self-hosted instances
```

### SafetyRailsConfig

```yaml
safetyRails:
  minCPU: "10m"
  minMemory: "64Mi"
  maxCPU: "8"
  maxMemory: "32Gi"
```

### PRSettingsConfig

```yaml
prSettings:
  cooldownPeriod: 168h  # Minimum time between PRs
  minChangePercent: 20  # Minimum change to create PR
  baseBranch: "main"
  branchPrefix: "sartor/"
  labels:
    - "automation"
    - "resource-optimization"
```

### ArgoCDConfig

```yaml
argocd:
  enabled: true
  serverURL: "https://argocd.example.com"  # Optional
  secretRef:
    name: argocd-token
    namespace: sartor-system
  namespace: "argocd"
  syncOnMerge: true
  refreshOnPRCreate: false
  hardRefresh: false
```

## Tailoring

The `Tailoring` resource is namespace-scoped and defines per-workload optimization.

### Spec Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `target` | TargetRef | Yes | Workload to optimize |
| `fitProfileRef` | FitProfileRef | Yes | FitProfile with the recommendation strategy and its parameters |
| `writeBack` | WriteBackConfig | Yes | GitOps configuration |
| `analysisWindow` | Duration | No | Time window for metrics (default from Atelier) |
| `paused` | bool | No | Dry-run mode (no PRs created) |

### TargetRef

```yaml
target:
  kind: Deployment  # Deployment, StatefulSet, DaemonSet
  name: my-app
  namespace: my-namespace  # Optional: defaults to the Tailoring's namespace; used in queries and write-back
  cluster: my-cluster      # Required when the Atelier sets prometheus.clusterLabel
```

### WriteBackConfig

```yaml
writeBack:
  type: raw  # raw, helm, kustomize
  repository: "org/repo"
  path: "apps/my-app/deployment.yaml"
  branch: "main"
```

## Status Fields

### AtelierStatus

- `prometheusConnected`: Connection status
- `gitProviderConnected`: Connection status
- `argoCDConnected`: Connection status (if enabled)
- `managedTailorings`: Count of active Tailorings
- `conditions`: Kubernetes conditions

### TailoringStatus

- `targetFound`: Whether target workload exists
- `vpaDetected`: Whether VPA is managing the workload
- `autoscalers`: HPAs, VPAs and KEDA ScaledObjects that target the workload, with their replica bounds, Utilization targets and other metrics
- `scaling`: How the recommendation was combined with the autoscalers: expected replica range, requests sized for HPA Utilization targets, spiky usage with recommended VPA bounds, conflicts and a short reason (see the [API reference](api-reference.md#scaling-summary))
- `oomKilledContainers`: Containers whose last termination was an OOM kill (metrics-only mode)
- `consecutiveFailures`: Failed analyses in a row; drives the retry backoff
- `nextAnalysisTime`: When the next analysis is scheduled
- `lastAnalysis`: Latest analysis results
  - `lastAnalysis.replicas`: Desired replica count of the target at analysis time, from kube-state-metrics; unset when not reported (see the [API reference](api-reference.md#tailoring-status-fields))
- `recommendedResources`: Current recommendations
- `lastCutRef`: Reference to most recent Cut
- `conditions`: Kubernetes conditions, including `FitProfileResolved` (`False` with reason `FitProfileFallback` when the FitProfile is missing, its parameters are invalid or its strategy is unknown) and `AutoscalerConflict` (`True` when two autoscalers act on the same resource)

### CutStatus

- `prState`: PR state (open/merged/closed)
- `prUrl`: URL to the pull request
- `prNumber`: PR number
- `commitSHA`: Git commit SHA
- `ignored`: Whether PR has sartor-ignore label
- `argoCDSyncTriggered`: Whether ArgoCD sync was triggered

## Examples

See [examples/](../examples/) directory for complete examples.
