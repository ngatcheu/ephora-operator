# Document d'Architecture Technique (DAT) — ephora-operator

> Version du document : **v0.1.x** (aligné sur les releases v0.1). Décisions d'architecture : [ADR-01 à ADR-06](adrs-ephora-operator.md).

## 1. Contexte et objectifs

### 1.1 Contexte
Les environnements de preview par Pull Request sont aujourd'hui soit absents (tests en staging partagé, source de conflits entre équipes), soit gérés par un pipeline CI fragile qui ne garantit pas le nettoyage des ressources après fermeture de la PR — d'où une fuite de coûts et une dérive progressive du cluster.

### 1.2 Objectifs
- Provisionner automatiquement un environnement isolé par Pull Request, à partir du chart Helm déjà maintenu par l'équipe applicative.
- Garantir la destruction automatique de l'environnement (TTL ou fermeture de la PR), sans intervention manuelle.
- Fournir une vue d'état self-service (statut, namespace, date d'expiration) consommable par les équipes et par la CI.
- Réduire le gaspillage de ressources cluster (argument FinOps mesurable).
- Traiter le chart de la PR comme une **entrée non fiable** : un chart malveillant ne doit pas pouvoir sortir de son environnement (ADR-05).

### 1.3 Hors périmètre (V1)
- Exposition publique (DNS/TLS dynamique) — reporté en V2 (cf. ADR-03).
- Gestion multi-cluster — l'opérateur cible un seul cluster de preview dédié en V1.
- Historisation/reporting FinOps avancé — les métriques Prometheus exposées suffisent en V1, le dashboard est hors scope de l'opérateur lui-même.
- Identifiants Git par équipe — un identifiant unique en lecture seule en V1 (cf. ADR-06).

## 2. Vue d'ensemble de l'architecture

```
┌──────────────┐  kubectl apply/delete   ┌──────────────────────────────────────────────────────┐
│ CI GitHub    │ ──────────────────────► │ Cluster Kubernetes                                   │
│ Actions      │  (compte CI limité aux  │                                                      │
│ (événement   │   PreviewEnvironment)   │  namespace de gestion        ephora-operator-system  │
│  de PR)      │                         │  ┌───────────────────┐       ┌────────────────────┐  │
└──────────────┘                         │  │ PreviewEnvironment│◄──────│ ephora-operator    │  │
                                         │  │ (CRD)             │ watch │ (controller-runtime│  │
       ┌───────────────────┐  git clone  │  └───────────────────┘       │  + SDK Helm)       │  │
       │ GitHub interne    │◄────────────┼───────────────────────────────┤                    │  │
       │ (chart de l'app,  │  jeton RO   │                               └─────────┬──────────┘  │
       │  révision de PR)  │             │          crée les garde-fous ;          │ helm install│
       └───────────────────┘             │          helm en impersonation          │ (identité   │
                                         │                                         ▼  ephora-    │
                                         │  ┌──────────────────────────────────────────┐ deployer)│
                                         │  │ namespace preview-pr-<n>-<app>           │          │
                                         │  │  - NetworkPolicy default-deny            │          │
                                         │  │  - ResourceQuota / LimitRange            │          │
                                         │  │  - SA ephora-deployer + RoleBinding      │          │
                                         │  │  - RoleBinding lecture (développeurs)    │          │
                                         │  │  - Release Helm de l'application         │          │
                                         │  └──────────────────────────────────────────┘          │
                                         └──────────────────────────────────────────────────────┘
```

### 2.1 Composants

| Composant | Rôle |
|---|---|
| **CRD `PreviewEnvironment`** (`penv`) | Contrat déclaratif décrivant l'environnement souhaité (source, TTL, overrides) |
| **Contrôleur** (Go, controller-runtime, scaffold Operator SDK) | Réconcilie la CRD : namespace et garde-fous, récupération du chart, déploiement via le **SDK Helm**, TTL, finalizer (cf. ADR-01 *Mise en œuvre*) |
| **Balayage des orphelins** | Tâche périodique du même processus (élection de leader) : supprime les namespaces gérés sans CRD associée, recalcule la jauge d'environnements actifs |
| **Namespace de l'opérateur** (`ephora-operator-system`) | Héberge l'opérateur |
| **Namespace de gestion** (ex. `ephora-previews`) | Contient les `PreviewEnvironment` ; seul endroit où la CI a des droits |
| **Namespace `preview-pr-<n>-<app>`** | Isolation par environnement (cf. ADR-04), supprimé à l'expiration |
| **Identité `ephora-deployer`** | ServiceAccount propre à chaque namespace de preview, emprunté par Helm (cf. ADR-05) |
| **CI GitHub Actions** | Déclenche création/mise à jour/suppression via `kubectl apply`/`delete` (cf. ADR-02) |

## 3. Spécification de l'API (CRD)

Groupe `ephora.io`, version `v1alpha1`, kind `PreviewEnvironment`, nom court `penv`. Cette section fait référence pour la liste des champs.

```yaml
apiVersion: ephora.io/v1alpha1
kind: PreviewEnvironment
metadata:
  name: pr-1234-checkout-api          # ≤ 53 caractères (nom de la release Helm)
  namespace: ephora-previews
spec:
  source:
    repo: "https://github.internal/checkout/checkout-api"
    chartPath: "charts/app"
    revision: "3f9c2a1"               # commit, branche ou tag
  prNumber: 1234
  appName: checkout-api
  valuesOverride:
    image:
      tag: "pr-1234-3f9c2a1"
    replicaCount: 1
  ttl: 48h
status:
  phase: Running                      # Pending | Provisioning | Running | Expiring | Terminating | Failed
  namespace: preview-pr-1234-checkout-api
  helmReleaseName: pr-1234-checkout-api
  observedRevision: "3f9c2a1"
  observedGeneration: 1
  expiresAt: "2026-09-21T18:06:00Z"
  conditions:
    - type: Ready
      status: "True"
      reason: HelmReleaseDeployed
      lastTransitionTime: "2026-09-19T18:06:00Z"
```

### 3.1 Champs du `spec`

| Champ | Type | Validation (schéma OpenAPI / CEL) |
|---|---|---|
| `source.repo` | string | Motif `^https://github\.internal/.+$` — dépôts internes uniquement (contrôle de sécurité, à adapter à l'hôte réel) |
| `source.chartPath` | string | Non vide ; doit rester dans le dépôt cloné (vérifié par le contrôleur) |
| `source.revision` | string | Motif `^[A-Za-z0-9][A-Za-z0-9._/-]*$`, ≤ 255 — ne peut pas commencer par `-` (injection d'option git) |
| `prNumber` | int32 | ≥ 1, **immuable** (`self == oldSelf`) |
| `appName` | string | Label DNS en minuscules, ≤ 40 |
| `valuesOverride` | object | Libre ; **aucun secret de production** |
| `ttl` | string | Heures entières (`^[0-9]{1,4}h$`), **≤ 168h** (CEL `duration(self) <= duration('168h')`) |
| `metadata.name` | string | ≤ 53 caractères (CEL au niveau de l'objet) |

### 3.2 Champs du `status`

| Champ | Description |
|---|---|
| `phase` | Phase de cycle de vie |
| `namespace` | Namespace de l'environnement (renseigné dès sa création, même en échec) |
| `helmReleaseName` | Release Helm (= `metadata.name`) |
| `observedRevision` | Dernière révision déployée avec succès |
| `observedGeneration` | Dernière `metadata.generation` déployée ; une différence déclenche un `helm upgrade` |
| `expiresAt` | `creationTimestamp + spec.ttl`, calculé une seule fois (une modification ultérieure du `ttl` ne prolonge pas l'environnement) |
| `cleanupStartedAt` | Début du nettoyage, pour le délai maximal du finalizer |
| `conditions[Ready]` | État détaillé (`reason`, `message`) |

## 4. Boucle de réconciliation

1. **Création** : ajout du finalizer `ephora.io/preview-cleanup`, puis dans le même passage :
   1. calcul de `status.expiresAt`, **avant** toute étape susceptible d'échouer (un environnement en échec expire quand même) ;
   2. création du namespace `preview-pr-<n>-<app>` avec les labels de propriété (`ephora.io/managed-by`, `ephora.io/owner`, `ephora.io/owner-namespace`). Un namespace existant qui appartient à un autre `PreviewEnvironment` est **refusé** (`NamespaceReconcileFailed`) ;
   3. `NetworkPolicy` default-deny, `ResourceQuota` et `LimitRange` ;
   4. identité de déploiement `ephora-deployer` (ServiceAccount + RoleBinding) et, si configuré, RoleBinding de lecture pour les développeurs ;
   5. clone du dépôt à la révision (jeton optionnel, cf. ADR-06 ; liens symboliques désactivés), chargement du chart, puis `helm install` **sous l'identité `ephora-deployer`** ;
   6. phase `Provisioning` puis `Running`.
2. **TTL** : la réconciliation est replanifiée à `min(expiresAt, 10 min)`. À l'échéance : phase `Expiring`, puis suppression de la CRD, qui déclenche le nettoyage (étape 4).
3. **Mise à jour** : tout changement du `spec` (`metadata.generation` ≠ `status.observedGeneration`) déclenche un `helm upgrade` (historique limité à 10 révisions). Une release bloquée (`pending-*`) ou jamais déployée est désinstallée puis réinstallée.
4. **Suppression** (CI ou TTL) : le finalizer exécute un `helm uninstall` (une seule tentative, tant que le namespace est intact), puis supprime le namespace et attend sa disparition. Au-delà de `--cleanup-timeout` (10 min par défaut), le finalizer est libéré et le balayage des orphelins prend le relais.
5. **Détection d'orphelins** : au démarrage puis toutes les 30 min, les namespaces gérés sans `PreviewEnvironment` associé sont supprimés.
6. **Correction de dérive** : un environnement `Running` est réconcilié au moins toutes les 10 min ; une NetworkPolicy, un quota ou un RoleBinding modifié ou supprimé à la main est rétabli.

**Déclenchement** : seuls les changements du `spec` et le début d'une suppression déclenchent une réconciliation (les écritures de `status` n'en déclenchent pas). Les erreurs sont réessayées avec un délai exponentiel (jusqu'à environ 16 min). Jusqu'à `--max-concurrent-reconciles` environnements (4 par défaut) sont traités en parallèle ; un même environnement ne l'est jamais deux fois simultanément.

## 5. Sécurité (angle DevSecOps)

| Aspect | Mesure |
|---|---|
| **RBAC CI** | Compte CI limité au rôle `previewenvironment-editor` dans le namespace de gestion — aucun accès aux namespaces `preview-*` |
| **RBAC opérateur** | CRD, namespaces, garde-fous, ServiceAccounts et RoleBindings ; `impersonate` limité aux comptes `ephora-deployer` ; `bind` limité aux ClusterRoles de déploiement et de lecture. **Aucun droit sur les ressources applicatives** |
| **Identité de déploiement** | Helm emprunte `ephora-deployer`, lié au ClusterRole `ephora-operator-preview-deployer` **dans le seul namespace** : pas d'action hors namespace, pas de `lookup` de Secrets ailleurs, pas d'Ingress, de RBAC ni de ressources cluster (ADR-05) |
| **Accès développeurs** | Lecture + `port-forward` par namespace (`--viewer-groups`), sans Secrets ni `exec` |
| **Isolation réseau** | `NetworkPolicy` default-deny ; entrée depuis le namespace et depuis `--preview-ingress-namespaces` (passerelle interne) ; sortie limitée au DNS et au cluster (cf. ADR-03) |
| **Quotas** | `ResourceQuota` (2 CPU / 4 Gi demandés, 4 CPU / 8 Gi max, 20 pods) et `LimitRange` par défaut |
| **Source des charts** | Liste blanche d'hôtes Git dans le schéma ; révision sans `-` initial ; `chartPath` confiné au dépôt ; liens symboliques non suivis (`core.symlinks=false`) |
| **Identifiant Git** | Jeton en lecture seule monté en fichier, transmis en en-tête limité à l'hôte du dépôt, jamais dans les logs ni le `status` (ADR-06) |
| **Image et pod** | Utilisateur non-root numérique, capacités supprimées, système de fichiers en lecture seule, seccomp `RuntimeDefault` |
| **Métriques** | HTTPS, authentification et autorisation (TokenReview + SubjectAccessReview) ; NetworkPolicy dédiée |
| **Secrets** | Aucun secret de production injecté dans les environnements de preview — valeurs de test/mock uniquement via `valuesOverride` |
| **Chaîne d'approvisionnement** | CI : golangci-lint, govulncheck, Trivy bloquant ; Dependabot. Release : image multi-architecture signée (cosign, sans clé), SBOM SPDX, provenance, `install.yaml` épinglé par digest |

## 6. Exigences non-fonctionnelles

| Exigence | Cible | Constaté (v0.1.1, kind) |
|---|---|---|
| **Délai de provisioning** | < 3 minutes entre application de la CRD et phase `Running` | 5,7 s (chart `hello-world`) |
| **Fiabilité du nettoyage** | 0 namespace orphelin après 1h au-delà du TTL | Finalizer + délai maximal + balayage (section 4) |
| **Observabilité** | Métriques Prometheus : `preview_environments_active`, `preview_environment_provisioning_duration_seconds`, `preview_environment_cleanup_total{reason}` | Exposées et vérifiées en cluster |
| **Scalabilité** | Au moins 50 environnements actifs simultanés sans dégradation | 50 créés d'un coup (`hack/loadtest.sh`, kind, sans pods) — avec 4 workers (défaut) : tous `Running` en 41 s, **p95 38 s**, nettoyage complet en 26 s, mémoire 95 Mio au pic. Avec 1 worker : 90 s, p95 83 s, 76 Mio. Au-delà de 4 workers, la limite devient l'environnement (clones réseau, API server) plutôt que l'opérateur |

## 7. Risques identifiés

| Risque | Impact | Mitigation |
|---|---|---|
| Chart malveillant ou compromis dans une PR | Lecture de secrets, déploiement hors namespace, fuite du jeton de l'opérateur | Identité de déploiement limitée au namespace (ADR-05), liens symboliques désactivés, rôle de déploiement restrictif |
| Finalizer bloqué (Helm uninstall échoue, namespace bloqué) | Namespace orphelin, CRD bloquée en suppression | Délai maximal sur le finalizer + balayage périodique des orphelins (section 4) |
| TTL mal configuré par un utilisateur (valeur trop longue) | Coût cluster non maîtrisé | Borne max (168h) sur `spec.ttl` au niveau du schéma |
| Chart applicatif consommant trop de ressources | Impact sur les autres environnements | `ResourceQuota` et `LimitRange` par namespace |
| Release Helm interrompue (redémarrage de l'opérateur) | Environnement bloqué en `pending-*` | Réinstallation automatique des releases bloquées |
| Double déclenchement CI (retry) | Erreur de réconciliation transitoire | `kubectl apply` idempotent ; écritures de `status` avec reprise sur conflit de version |
| Fuite de l'identifiant Git | Accès en lecture aux dépôts | Jeton en lecture seule, jamais exposé (ADR-06), révocable ; rotation sans redémarrage |
| Vulnérabilité dans une dépendance | Image vulnérable | Trivy bloquant en CI et en release, govulncheck, Dependabot |

## 8. Roadmap V2 (hors périmètre actuel)

À activer derrière une option, sans modifier le comportement V1 par défaut :
- Exposition via Ingress dynamique + cert-manager (révision de l'ADR-03).
- Commentaire automatique sur la PR GitHub avec le lien et le statut de l'environnement.
- Dashboard FinOps agrégeant les métriques d'usage cumulé des environnements éphémères.
- Identifiants Git par équipe (révision de l'ADR-06).
- Métrique par phase (environnements `Failed`).

## 9. Références

- [ADRs](adrs-ephora-operator.md) : ADR-01 à ADR-06
- [Guide d'onboarding](onboarding.md) : intégration d'un dépôt applicatif
- [Runbook d'exploitation](runbook.md) : installation, surveillance, incidents
- [README](../README.md) : démarrage rapide, référence d'API, développement
