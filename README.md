# ephora-operator

[![CI](https://github.com/ngatcheu/ephora-operator/actions/workflows/ci.yml/badge.svg)](https://github.com/ngatcheu/ephora-operator/actions/workflows/ci.yml)
[![Kubernetes](https://img.shields.io/badge/kubernetes-1.28%2B-326ce5.svg)](https://kubernetes.io)
[![Go](https://img.shields.io/badge/go-1.26-00ADD8.svg)](https://go.dev)
[![Status](https://img.shields.io/badge/status-alpha-yellow.svg)](#feuille-de-route)

**ephora-operator** est un opérateur Kubernetes qui gère tout le cycle de vie des **environnements de preview éphémères** : pour chaque Pull Request, il crée un namespace isolé, y déploie le chart Helm de l'application, suit son état, puis supprime tout automatiquement à l'expiration du TTL ou à la fermeture de la PR.

> Plus d'environnements oubliés, plus de nettoyage manuel : on applique une CRD, on obtient un environnement ; on la supprime (ou on la laisse expirer), tout disparaît.

---

## Sommaire

- [Pourquoi](#pourquoi)
- [Fonctionnalités](#fonctionnalités)
- [Fonctionnement](#fonctionnement)
- [Démarrage rapide (kind)](#démarrage-rapide-kind)
- [Utilisation](#utilisation)
- [Référence de l'API](#référence-de-lapi)
- [Sécurité](#sécurité)
- [Observabilité](#observabilité)
- [Configuration de l'opérateur](#configuration-de-lopérateur)
- [Déploiement dans un cluster](#déploiement-dans-un-cluster)
- [Développement](#développement)
- [Feuille de route](#feuille-de-route)

---

## Pourquoi

Un **environnement de preview éphémère** est une copie temporaire et isolée de l'application, créée automatiquement pour **une Pull Request**, puis détruite quand elle n'est plus utile.

- **Preview** : il contient exactement le code de la PR. On peut **tester la modification dans un vrai Kubernetes avant de la fusionner** — le développeur valide son code, le relecteur teste la fonctionnalité au lieu de seulement lire le diff.
- **Éphémère** : il naît à l'ouverture de la PR, se met à jour à chaque commit, et disparaît à la fermeture de la PR ou à l'expiration de son TTL.

**Exemple** : Alice ouvre la PR #1234 sur `checkout-api` → la CI crée `preview-pr-1234-checkout-api` avec la version de la PR → Bob la teste → Alice pousse un correctif, l'environnement est mis à jour → la PR est fusionnée, l'environnement est supprimé. Pendant ce temps, la PR #1235 a son propre environnement, sans interférence.

| Sans | Avec ephora-operator |
|---|---|
| Un **staging partagé** où les PR s'écrasent mutuellement | **Un environnement isolé par PR** |
| Les bugs sont découverts **après** la fusion | On teste **avant** la fusion |
| Des environnements **oubliés** qui coûtent en continu | **Suppression automatique**, zéro orphelin |
| Un pipeline CI fragile qui crée et nettoie à la main | Un **contrat déclaratif** : la CI déclare, l'opérateur s'occupe du reste |

On parle aussi de *review apps* (GitLab, Heroku) ou d'*ephemeral environments*.

## Fonctionnalités

- 🚀 **Provisionnement déclaratif** — une CRD `PreviewEnvironment` par Pull Request, qui déploie le chart Helm existant de l'application depuis son dépôt Git, à la révision demandée.
- 🔄 **Mise à jour continue** — toute modification du `spec` (nouveau commit, `valuesOverride`…) déclenche un `helm upgrade`.
- ⏱ **Expiration automatique** — TTL borné à 7 jours ; même un environnement en échec expire.
- 🧹 **Nettoyage garanti** — finalizer Kubernetes + balayage périodique des namespaces orphelins.
- 🔒 **Isolé par défaut** — namespace dédié, `NetworkPolicy` default-deny, `ResourceQuota` et `LimitRange`.
- 📊 **Observable** — métriques Prometheus, conditions et événements Kubernetes.
- 🔁 **Compatible GitOps** — la CI applique/supprime la CRD via `kubectl`, aucun endpoint HTTP à exposer ni à sécuriser.

## Fonctionnement

```
CI (GitLab / GitHub)  ──kubectl apply/delete──▶  PreviewEnvironment (CRD)
                                                       │
                                                       ▼
                                              ephora-operator (reconciler)
                                                       │
                         ┌─────────────────────────────┼──────────────────────────┐
                         ▼                             ▼                          ▼
          namespace preview-pr-<n>-<app>     git clone du chart         TTL (requeue) +
          - NetworkPolicy default-deny       → helm install/upgrade     finalizer de nettoyage
          - ResourceQuota / LimitRange
```

**Cycle de vie** (`status.phase`) :

| Phase | Signification |
|---|---|
| `Provisioning` | Namespace et garde-fous créés, déploiement du chart en cours |
| `Running` | Release Helm déployée |
| `Failed` | Erreur (clone, chart, Helm…) — réessai automatique avec backoff exponentiel ; la raison est dans la condition `Ready` |
| `Expiring` | TTL atteint, suppression déclenchée |
| `Terminating` | Suppression en cours (désinstallation Helm puis suppression du namespace) |

**Détails de la réconciliation :**

1. Ajout du finalizer `ephora.io/preview-cleanup`.
2. Calcul de `status.expiresAt = création + spec.ttl` (une seule fois, non recalculé si le TTL change).
3. Création du namespace `preview-pr-<prNumber>-<appName>` et de ses garde-fous.
4. Clone de `spec.source.repo` à `spec.source.revision`, chargement du chart `spec.source.chartPath`, puis `helm install` ou `helm upgrade` avec `spec.valuesOverride`.
5. À l'expiration ou à la suppression de la CRD : `helm uninstall`, suppression du namespace, puis retrait du finalizer (après au plus `--cleanup-timeout` ; le balayage des orphelins prend le relais si besoin).

> **Choix d'architecture** : le projet est scaffoldé avec Operator SDK (plugin Helm), mais la réconciliation est écrite en Go : le chart à déployer est dynamique par instance, ce que le reconciler Helm générique ne sait pas faire. Toute la logique de déploiement reste déléguée au SDK Helm (install/upgrade/uninstall). Voir ADR-01 dans [adrs-ephora-operator.md](adrs-ephora-operator.md).

## Démarrage rapide (kind)

Prérequis : les outils de la section [Développement](#prérequis).

```bash
# 1. Cluster local
kind create cluster --name ephora

# 2. CRD de développement (autorise le dépôt public helm/examples, voir plus bas)
make install-dev

# 3. Opérateur en local (terminal 1, à laisser tourner)
make run

# 4. Environnement de test (terminal 2)
kubectl apply -f config/dev/samples/hello-world.yaml
kubectl get penv -w
```

Au bout de quelques secondes, l'environnement passe en `Running` :

```bash
$ kubectl get penv
NAME               PHASE     PR   NAMESPACE                  EXPIRES                AGE
pr-1-hello-world   Running   1    preview-pr-1-hello-world   2026-09-26T14:10:05Z   1m

$ kubectl get pods -n preview-pr-1-hello-world
NAME                               READY   STATUS    RESTARTS   AGE
pr-1-hello-world-755897b8d-qlzt9   1/1     Running   0          1m
```

Nettoyage :

```bash
kubectl delete penv pr-1-hello-world   # le namespace est supprimé par le finalizer
kind delete cluster --name ephora
```

> ⚠️ **`make install-dev` est réservé au développement.** Il installe une variante de la CRD qui accepte en plus `https://github.com/helm/examples` (les hôtes internes restent autorisés, tout le reste reste refusé). La cible refuse de s'exécuter si le contexte kube n'est pas un cluster kind. En production, utilisez `make install`.

## Utilisation

### Créer un environnement

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
    replicaCount: 1
  ttl: 48h
```

```bash
kubectl apply -f preview-pr-1234.yaml
kubectl get penv pr-1234-checkout-api -w      # "penv" est le nom court
```

### Suivre l'état

```bash
kubectl get penv pr-1234-checkout-api -o jsonpath='{.status}' | jq
kubectl describe penv pr-1234-checkout-api      # conditions + événements
```

```yaml
status:
  phase: Running
  namespace: preview-pr-1234-checkout-api
  helmReleaseName: pr-1234-checkout-api
  observedRevision: pr-1234-abc123
  observedGeneration: 1
  expiresAt: "2026-09-28T12:06:27Z"
  conditions:
    - type: Ready
      status: "True"
      reason: HelmReleaseDeployed
```

### Mettre à jour

Toute modification du `spec` déclenche un `helm upgrade` :

```bash
kubectl patch penv pr-1234-checkout-api --type merge \
  -p '{"spec":{"source":{"revision":"pr-1234-def456"}}}'
```

### Supprimer

Laisser expirer le TTL, ou supprimer la CRD (typiquement fait par la CI à la fermeture de la PR) :

```bash
kubectl delete penv pr-1234-checkout-api
```

### Intégration CI (exemple GitLab)

```yaml
deploy_preview:
  stage: preview
  script:
    - envsubst < preview-template.yaml | kubectl apply -f -
  rules:
    - if: '$CI_PIPELINE_SOURCE == "merge_request_event"'

cleanup_preview:
  stage: preview
  script:
    - kubectl delete previewenvironment pr-${CI_MERGE_REQUEST_IID}-checkout-api --ignore-not-found
  rules:
    - if: '$CI_PIPELINE_SOURCE == "merge_request_event"'
      when: manual
```

Le compte de service de la CI n'a besoin que des droits sur les `PreviewEnvironment` du namespace de gestion (rôle `previewenvironment-editor` fourni dans `config/rbac/`), jamais d'un accès direct aux namespaces `preview-*`.

## Référence de l'API

Groupe `ephora.io`, version `v1alpha1`, kind `PreviewEnvironment` (nom court `penv`).

### Spec

| Champ | Type | Obligatoire | Validation | Description |
|---|---|---|---|---|
| `source.repo` | string | ✅ | `^https://(gitlab\.internal\|github\.internal)/.+$` | Dépôt Git du chart — hôtes internes uniquement |
| `source.chartPath` | string | ✅ | non vide, doit rester dans le dépôt | Chemin du chart dans le dépôt |
| `source.revision` | string | ✅ | `^[A-Za-z0-9][A-Za-z0-9._/-]*$`, ≤ 255 | Commit, branche ou tag |
| `prNumber` | int | ✅ | ≥ 1, **immuable** | Numéro de la Pull Request |
| `appName` | string | ✅ | label DNS en minuscules, ≤ 40 | Nom de l'application |
| `valuesOverride` | object | | libre | Values Helm appliquées par-dessus celles du chart |
| `ttl` | string | ✅ | heures entières (`48h`), **≤ 168h** | Durée de vie avant suppression |
| `metadata.name` | string | ✅ | ≤ 53 caractères | Sert de nom de release Helm |

> **Adapter `source.repo`** : le motif par défaut n'accepte que les hôtes d'exemple `gitlab.internal` / `github.internal`. Remplacez-le par vos vrais hôtes Git internes dans `api/v1alpha1/previewenvironment_types.go`, puis `make manifests`.

### Status

| Champ | Description |
|---|---|
| `phase` | `Pending` \| `Provisioning` \| `Running` \| `Expiring` \| `Terminating` \| `Failed` |
| `namespace` | Namespace de l'environnement |
| `helmReleaseName` | Nom de la release Helm |
| `observedRevision` | Dernière révision déployée avec succès |
| `observedGeneration` | Dernière génération du `spec` déployée (différente de `metadata.generation` → upgrade) |
| `expiresAt` | Date d'expiration calculée |
| `cleanupStartedAt` | Début du nettoyage (pour le délai maximal du finalizer) |
| `conditions[Ready]` | État détaillé : `reason` + `message` en cas d'erreur |

## Sécurité

| Mesure | Détail |
|---|---|
| **Isolation réseau** | `NetworkPolicy` `ephora-default` dans chaque namespace : entrée depuis le namespace et le cluster, sortie limitée au DNS et au cluster — pas d'Internet |
| **Quotas** | `ResourceQuota` (2 CPU / 4 Gi demandés, 4 CPU / 8 Gi max, 20 pods) + `LimitRange` (valeurs par défaut par conteneur) |
| **Source des charts** | Liste blanche d'hôtes Git dans le schéma de la CRD |
| **Injection d'arguments git** | Révision commençant par `-` refusée (CRD + code), `git checkout <rev> --` |
| **Traversée de chemin** | `chartPath` sortant du dépôt cloné (`../..`) refusé |
| **Coût** | TTL borné à 168h au niveau du schéma |
| **Image** | Utilisateur non-root numérique (65532), capacités supprimées, système de fichiers en lecture seule (`/tmp` en `emptyDir` de 1 Gi) |
| **RBAC** | Rôle généré (`role.yaml`) pour la CRD/namespaces/garde-fous + rôle maintenu à la main (`role_helm_workloads.yaml`) pour les ressources des charts — sans Ingress ni RBAC |
| **Secrets** | Aucun secret de production dans `valuesOverride` ni dans les namespaces de preview |
| **Supply chain** | CI : `govulncheck`, scan Trivy de l'image (bloquant sur HIGH/CRITICAL corrigeables) et des manifestes ; Dependabot |

**Limites connues** (à traiter avant la production) :

- Le rôle Helm donne accès aux `secrets` **sur tout le cluster** → à restreindre aux namespaces `preview-*`.
- La `NetworkPolicy` accepte l'entrée depuis **tous** les namespaces → à limiter à la passerelle interne (VPN, ingress interne).
- Pas encore d'authentification Git : seuls les dépôts accessibles sans identifiants sont clonables.

Détails : [DAT §5](dat-ephora-operator.md) et [ADRs](adrs-ephora-operator.md).

## Observabilité

Métriques Prometheus exposées sur `:8080/metrics` :

| Métrique | Type | Description |
|---|---|---|
| `preview_environments_active` | gauge | Environnements en phase `Running` (recalculé à chaque balayage) |
| `preview_environment_provisioning_duration_seconds` | histogram | Délai création → premier `Running` (objectif < 3 min) |
| `preview_environment_cleanup_total{reason}` | counter | Nettoyages par raison : `deleted`, `ttl_expired`, `orphan_sweep` |

Les erreurs sont aussi remontées en **événements Kubernetes** (`kubectl describe penv …`) et dans la condition `Ready`.

## Configuration de l'opérateur

| Flag | Défaut | Description |
|---|---|---|
| `--metrics-bind-address` | `:8080` | Adresse des métriques |
| `--health-probe-bind-address` | `:8081` | Sondes `/healthz` et `/readyz` |
| `--leader-elect` | `false` | Élection de leader (obligatoire au-delà d'un réplica) |
| `--orphan-sweep-interval` | `30m` | Fréquence du balayage des namespaces orphelins |
| `--cleanup-timeout` | `10m` | Attente maximale de la suppression du namespace avant de libérer le finalizer |
| `--zap-devel`, `--zap-log-level`… | | Options de logs ([zap](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/log/zap)) |

## Déploiement dans un cluster

```bash
# Image
make docker-build IMG=<registry>/ephora-operator:v0.1.0
make docker-push  IMG=<registry>/ephora-operator:v0.1.0
# (kind : kind load docker-image <registry>/ephora-operator:v0.1.0 --name ephora)

# CRD + RBAC + opérateur dans le namespace ephora-operator-system
make deploy IMG=<registry>/ephora-operator:v0.1.0

# Désinstallation
make undeploy
```

## Développement

### Prérequis

| Outil | Version | Usage |
|---|---|---|
| [Go](https://go.dev/dl/) | 1.26 (dernier correctif) | Compilation, tests |
| GNU `make` | — | Toutes les cibles |
| [Docker](https://docs.docker.com/get-docker/) | récent | Image, kind |
| [kind](https://kind.sigs.k8s.io/) | récent | Cluster local |
| [kubectl](https://kubernetes.io/docs/tasks/tools/) | compatible avec le cluster | |
| [Helm](https://helm.sh/docs/intro/install/) | v3 | Inspection des releases |
| [golangci-lint](https://golangci-lint.run/) | v2 | Lint |
| git | ≥ 2.24 | Clone des charts (aussi requis à l'exécution) |

`controller-gen`, `setup-envtest` et `kustomize` sont téléchargés automatiquement dans `./bin` par le Makefile.

**Linux / WSL (recommandé)** — installe toute la boîte à outils d'opérateurs dans `~/.local`, sans `sudo` (réutilisable pour d'autres projets) :

```bash
bash hack/install-dev-tools.sh            # installe ce qui manque
bash hack/install-dev-tools.sh --upgrade  # met tout à jour
source ~/.bashrc
```

**Windows** (PowerShell administrateur, via Chocolatey) :

```powershell
powershell -ExecutionPolicy Bypass -File hack\install-dev-tools.ps1
```

> Sous WSL, travaillez de préférence dans le système de fichiers Linux (`~/…`) plutôt que sous `/mnt/c` : la compilation Go y est nettement plus rapide.

### Commandes

| Commande | Description |
|---|---|
| `make run` | Lance l'opérateur en local contre le contexte kube courant (logs lisibles) |
| `make test` | Tests unitaires + envtest (API server local) |
| `make install` / `make uninstall` | Installe / retire la CRD |
| `make install-dev` | CRD de développement (kind uniquement) |
| `make manifests` | Régénère CRD et `role.yaml` depuis les marqueurs `+kubebuilder` |
| `make generate` | Régénère `zz_generated.deepcopy.go` |
| `make build` | Compile `bin/manager` |
| `make docker-build IMG=…` | Construit l'image |
| `make deploy IMG=…` / `make undeploy` | Déploie / retire l'opérateur dans le cluster |
| `golangci-lint run ./...` | Lint |

> Ne modifiez jamais `config/rbac/role.yaml` à la main : il est régénéré par `make manifests`. Les droits des charts se trouvent dans `config/rbac/role_helm_workloads.yaml`.

### Tests

```bash
make test
```

- **Unitaires** : nom du namespace, décodage des values, récupération du chart depuis un dépôt Git local (injection d'options, traversée de chemin, nettoyage des fichiers temporaires).
- **envtest** : provisionnement des garde-fous, chemin `Failed`, suppression par finalizer, expiration TTL, métriques, et règles de validation de la CRD.

Les tests sont **hors ligne** (`GIT_ALLOW_PROTOCOL=file`) : aucun accès réseau.

### CI

[`.github/workflows/ci.yml`](.github/workflows/ci.yml), sur chaque Pull Request et sur `main` :

| Job | Contenu |
|---|---|
| Test | `make test` + vérification que les fichiers générés sont commités |
| Lint | `golangci-lint` + `govulncheck` |
| Image | Build Docker + Trivy (image bloquant, manifestes informatif) → onglet *Security* |

### Structure du projet

```
api/v1alpha1/            Types de la CRD (spec, status, validations)
cmd/main.go              Point d'entrée (manager, flags)
internal/controller/
  previewenvironment_controller.go   Réconciliation, TTL, finalizer
  namespace.go                       Namespace, NetworkPolicy, quotas
  helmchart.go                       Clone Git + SDK Helm
  orphansweeper.go                   Balayage des namespaces orphelins
  metrics.go                         Métriques Prometheus
config/
  crd/        CRD générée             rbac/     Rôles (générés + Helm)
  manager/    Déploiement             default/  Kustomize de déploiement
  dev/        CRD de dev + exemple    samples/  Exemple de PreviewEnvironment
hack/                    Scripts d'installation des outils
```

### Documentation d'architecture

- [dat-ephora-operator.md](dat-ephora-operator.md) — Document d'architecture technique (composants, API, réconciliation, sécurité, exigences non fonctionnelles)
- [adrs-ephora-operator.md](adrs-ephora-operator.md) — Décisions d'architecture (ADR-01 à ADR-04)

## Feuille de route

**V1 (en cours)**

- [x] CRD, réconciliation, TTL, finalizer, balayage des orphelins
- [x] Garde-fous par namespace (NetworkPolicy, quotas)
- [x] Tests unitaires + envtest, CI DevSecOps
- [ ] Restreindre le RBAC Helm et la NetworkPolicy
- [ ] Authentification Git pour les dépôts privés
- [ ] Test de bout en bout de l'opérateur déployé dans le cluster

**V2** (derrière un flag, hors du chemin V1 par défaut)

- [ ] Ingress dynamique + cert-manager (ADR-03)
- [ ] Commentaire automatique sur la PR avec l'état de l'environnement
- [ ] Tableau de bord FinOps

## Licence

Apache 2.0
