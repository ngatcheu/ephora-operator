# Document d'Architecture Technique (DAT) — ephora-operator

## 1. Contexte et objectifs

### 1.1 Contexte
Les environnements de preview par Pull Request sont aujourd'hui soit absents (tests en staging partagé, source de conflits entre équipes), soit gérés par un pipeline CI fragile qui ne garantit pas le nettoyage des ressources après fermeture de la PR — d'où une fuite de coûts et une dérive progressive du cluster.

### 1.2 Objectifs
- Provisionner automatiquement un environnement isolé par Pull Request, à partir du chart Helm déjà maintenu par l'équipe applicative.
- Garantir la destruction automatique de l'environnement (TTL ou fermeture de la PR), sans intervention manuelle.
- Fournir une vue d'état self-service (statut, namespace, date d'expiration) consommable par les équipes et par la CI.
- Réduire le gaspillage de ressources cluster (argument FinOps mesurable).

### 1.3 Hors périmètre (V1)
- Exposition publique (DNS/TLS dynamique) — reporté en V2 (cf. ADR-03).
- Gestion multi-cluster — l'opérateur cible un seul cluster de preview dédié en V1.
- Historisation/reporting FinOps avancé — les métriques Prometheus exposées suffisent en V1, le dashboard est hors scope de l'opérateur lui-même.

## 2. Vue d'ensemble de l'architecture

```
┌────────────┐   kubectl apply/delete CRD   ┌──────────────────────────┐
│  CI GitLab │ ───────────────────────────► │  Cluster Kubernetes       │
│  (PR event)│                               │  (namespace de gestion)   │
└────────────┘                               │                            │
                                              │  ┌──────────────────────┐ │
                                              │  │ Ephemeral Environment │ │
                                              │  │ Operator (Operator    │ │
                                              │  │ SDK, plugin Helm)     │ │
                                              │  └──────────┬───────────┘ │
                                              │             │ réconcilie   │
                                              │             ▼              │
                                              │  ┌──────────────────────┐ │
                                              │  │ namespace preview-*   │ │
                                              │  │  - Release Helm app   │ │
                                              │  │  - NetworkPolicy      │ │
                                              │  │  - ResourceQuota      │ │
                                              │  └──────────────────────┘ │
                                              └──────────────────────────┘
```

### 2.1 Composants

| Composant | Rôle |
|---|---|
| **CRD `PreviewEnvironment`** | Contrat déclaratif décrivant l'environnement souhaité (source, TTL, overrides) |
| **Contrôleur (Operator SDK / Helm)** | Réconcilie le CRD : déploie/détruit le chart, gère le namespace, applique le TTL |
| **Namespace de gestion** | Héberge l'opérateur et les CRD ; distinct des namespaces `preview-*` |
| **Namespace `preview-pr-<n>-<app>`** | Isolation par environnement (cf. ADR-04), supprimé à l'expiration |
| **CI GitLab** | Déclenche création/suppression via `kubectl apply`/`delete` (cf. ADR-02) |

## 3. Spécification de l'API (CRD)

```yaml
apiVersion: ephora.io/v1alpha1
kind: PreviewEnvironment
metadata:
  name: pr-1234-checkout-api
spec:
  source:
    repo: "https://gitlab.internal/checkout/checkout-api"
    chartPath: "charts/app"
    revision: "pr-1234-abc123"
  prNumber: 1234
  appName: checkout-api
  valuesOverride:
    image:
      tag: "pr-1234-abc123"
    replicas: 1
  ttl: 48h
status:
  phase: Pending          # Pending | Provisioning | Running | Expiring | Terminating | Failed
  namespace: preview-pr-1234-checkout-api
  helmReleaseName: pr-1234-checkout-api
  expiresAt: "2026-09-21T18:06:00Z"
  conditions:
    - type: Ready
      status: "True"
      reason: HelmReleaseDeployed
      lastTransitionTime: "2026-09-19T18:06:00Z"
```

### 3.1 Validation (CRD OpenAPI schema)
- `spec.ttl` : format durée Go (`1h`, `48h`), borne max configurable (ex. 7 jours) pour éviter une dérive de coût si mal renseigné.
- `spec.prNumber` : entier positif, immutable après création (`x-kubernetes-validations` ou webhook de validation).
- `spec.source.repo` : validé par pattern (URL Git interne autorisée uniquement — évite qu'un CRD pointe vers un dépôt externe non maîtrisé).

## 4. Boucle de réconciliation

1. **Création** : le contrôleur détecte le nouveau CRD → crée le namespace `preview-pr-<n>-<app>` → applique un `ResourceQuota`/`LimitRange` par défaut → déploie le chart Helm référencé avec `spec.valuesOverride` → passe `.status.phase` à `Provisioning` puis `Running` une fois le Helm release `deployed`.
2. **TTL** : à la création, le contrôleur calcule `.status.expiresAt = now + spec.ttl` et programme un `requeue` à cette échéance. À l'échéance, passage en phase `Expiring`, puis déclenchement de la destruction.
3. **Mise à jour** : si `spec.valuesOverride` ou `spec.source.revision` change (nouveau commit sur la PR), le contrôleur relance un `helm upgrade` sur la release existante.
4. **Suppression** (CI ou TTL) : un **finalizer** (`ephora.io/preview-cleanup`) garantit que le namespace et la release Helm sont supprimés avant que le CRD ne soit effectivement retiré de l'API — évite les ressources orphelines.
5. **Détection d'orphelins** : une réconciliation périodique (ex. toutes les 30 min) liste les namespaces `preview-*` sans CRD `PreviewEnvironment` associé et les marque pour nettoyage — filet de sécurité si un finalizer a échoué.

## 5. Sécurité (angle DevSecOps)

| Aspect | Mesure |
|---|---|
| **RBAC CI** | Service account CI scopé uniquement au CRD `PreviewEnvironment` dans le namespace de gestion — pas d'accès direct aux namespaces `preview-*` |
| **RBAC opérateur** | ClusterRole limité à : gestion des namespaces `preview-*` (create/delete), déploiement Helm (via `kubernetes.core.helm`), lecture des CRD `PreviewEnvironment` |
| **Isolation réseau** | `NetworkPolicy` deny-all par défaut appliquée à chaque namespace `preview-*`, avec exceptions minimales (accès interne autorisé, cf. ADR-03) |
| **Quotas** | `ResourceQuota`/`LimitRange` par défaut pour éviter qu'un environnement de preview consomme des ressources disproportionnées |
| **Source des charts** | `spec.source.repo` restreint aux dépôts Git internes connus (pattern de validation), pour éviter le déploiement d'un chart arbitraire non maîtrisé |
| **Secrets** | Aucun secret de production injecté dans les environnements de preview — valeurs de test/mock uniquement via `valuesOverride` |

## 6. Exigences non-fonctionnelles

| Exigence | Cible |
|---|---|
| **Délai de provisioning** | < 3 minutes entre application du CRD et phase `Running` |
| **Fiabilité du nettoyage** | 0 namespace orphelin après 1h au-delà du TTL (garanti par la détection périodique, section 4.5) |
| **Observabilité** | Métriques Prometheus : `preview_environments_active`, `preview_environment_provisioning_duration_seconds`, `preview_environment_cleanup_total` |
| **Scalabilité** | Support d'au moins 50 environnements actifs simultanés sans dégradation de la boucle de réconciliation |

## 7. Risques identifiés

| Risque | Impact | Mitigation |
|---|---|---|
| Finalizer bloqué (Helm uninstall échoue) | Namespace orphelin, CRD bloqué en `Terminating` | Timeout sur le finalizer + réconciliation périodique de nettoyage forcé (section 4.5) |
| TTL mal configuré par un utilisateur (valeur trop longue) | Coût cluster non maîtrisé | Borne max sur `spec.ttl` au niveau du schema CRD |
| Chart applicatif buggé consommant trop de ressources | Impact sur les autres environnements du cluster de preview | `ResourceQuota` par namespace (section 5) |
| Double déclenchement CI (retry) créant un conflit sur le même CRD | Erreur de réconciliation transitoire | `kubectl apply` est idempotent par nature ; le contrôleur doit gérer les conflits de version (`resourceVersion`) proprement |

## 8. Roadmap V2 (hors périmètre actuel)

- Exposition publique via Ingress dynamique + cert-manager (lié à ADR-03).
- Intégration d'un commentaire automatique sur la PR GitLab avec le lien/statut de l'environnement.
- Dashboard FinOps agrégeant les métriques d'usage cumulé des environnements éphémères.

## 9. Références

- ADR-01 à ADR-04 : voir `adrs-ephemeral-environment-operator.md`
- Comparatif Kubebuilder / Operator SDK : ADR de cadrage général du choix de framework
