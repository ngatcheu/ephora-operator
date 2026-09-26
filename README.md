# ephora-operator

[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Kubernetes](https://img.shields.io/badge/kubernetes-1.28%2B-326ce5.svg)](https://kubernetes.io)
[![Operator SDK](https://img.shields.io/badge/operator--sdk-helm--plugin-orange.svg)](https://sdk.operatorframework.io)
[![Status](https://img.shields.io/badge/status-alpha-yellow.svg)](#)

**ephora-operator** est un opérateur Kubernetes qui automatise tout le cycle de vie des **environnements de preview éphémères** : il provisionne un namespace isolé et une release Helm par Pull Request, suit leur état, et les supprime automatiquement à l'expiration du TTL ou à la fermeture de la PR — fini les environnements orphelins et le nettoyage manuel.

Construit avec [Operator SDK](https://sdk.operatorframework.io) (plugin Helm).

---

## Pourquoi

Les environnements de preview par PR sont en général soit absents (un staging partagé, source de conflits), soit gérés par un pipeline CI fragile qui ne fait pas le ménage de façon fiable — d'où des namespaces orphelins et un coût de cluster gaspillé.

`ephora-operator` transforme cela en un contrat déclaratif qui se nettoie tout seul : on applique une CRD, on obtient un environnement qui tourne ; on la laisse expirer (ou on supprime la CRD), et tout ce qu'elle a créé disparaît avec elle.

## Fonctionnalités

- 🚀 **Provisionnement déclaratif** — une CRD (`PreviewEnvironment`) par Pull Request, qui s'appuie sur le chart Helm existant de votre application.
- ⏱ **Expiration automatique par TTL** — aucun environnement qui traîne après une PR oubliée.
- 🧹 **Nettoyage garanti** — finalizer Kubernetes + détection périodique des namespaces orphelins.
- 🔒 **Isolé par défaut** — namespace dédié par environnement, `NetworkPolicy` default-deny, `ResourceQuota`/`LimitRange`.
- 📊 **Observable** — métriques Prometheus sur les environnements actifs, la durée de provisionnement et les nettoyages.
- 🔁 **Compatible GitOps** — pas de boucle de réconciliation concurrente ; la CI applique/supprime directement la CRD, aucun endpoint HTTP supplémentaire à sécuriser.

## Fonctionnement

```
CI (GitLab)  ──kubectl apply/delete──▶  CRD PreviewEnvironment
                                              │
                                              ▼
                                    ephora-operator (reconciler)
                                              │
                          ┌───────────────────┴───────────────────┐
                          ▼                                       ▼
              namespace : preview-pr-<n>-<app>         planificateur TTL (requeue)
              - release Helm (chart de votre app)
              - NetworkPolicy (deny-all par défaut)
              - ResourceQuota / LimitRange
```

## Démarrage rapide

### Prérequis

- Un cluster Kubernetes (1.28+)
- Helm 3
- Un accès `kubectl` avec les droits pour créer la CRD `PreviewEnvironment`

### Installer l'opérateur

```bash
kubectl apply -f https://raw.githubusercontent.com/<org>/ephora-operator/main/dist/install.yaml
```

### Créer un environnement de preview

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
```

```bash
kubectl apply -f preview-pr-1234.yaml
kubectl get previewenvironment pr-1234-checkout-api -w
```

### Vérifier l'état

```bash
kubectl get previewenvironment pr-1234-checkout-api -o yaml
```

```yaml
status:
  phase: Running
  namespace: preview-pr-1234-checkout-api
  helmReleaseName: pr-1234-checkout-api
  expiresAt: "2026-09-21T18:06:00Z"
  conditions:
    - type: Ready
      status: "True"
      reason: HelmReleaseDeployed
```

### Supprimer l'environnement

Soit on laisse le TTL expirer, soit on supprime directement la CRD (en général fait par la CI à la fermeture de la PR) :

```bash
kubectl delete previewenvironment pr-1234-checkout-api
```

## Intégration CI (exemple GitLab)

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
    - if: '$CI_MERGE_REQUEST_EVENT_TYPE == "detached"'
      when: never
    - if: '$CI_PIPELINE_SOURCE == "merge_request_event"'
      when: manual
```

## Référence de l'API

| Champ | Type | Description |
|---|---|---|
| `spec.source.repo` | string | URL du dépôt Git qui héberge le chart Helm de l'application (dépôts internes uniquement) |
| `spec.source.chartPath` | string | Chemin du chart dans le dépôt |
| `spec.source.revision` | string | Révision du chart/de l'app à déployer (SHA de commit, tag) |
| `spec.prNumber` | int | Numéro de la Pull Request (immuable) |
| `spec.appName` | string | Nom de l'application, utilisé pour construire le namespace |
| `spec.valuesOverride` | object | Surcharge des values Helm appliquée par-dessus les valeurs par défaut du chart |
| `spec.ttl` | duration | Durée de vie avant suppression automatique (ex. `48h`) |
| `status.phase` | string | `Pending` \| `Provisioning` \| `Running` \| `Expiring` \| `Terminating` \| `Failed` |
| `status.namespace` | string | Namespace créé pour cet environnement |
| `status.expiresAt` | timestamp | Date d'expiration calculée |

## Modèle de sécurité

- `NetworkPolicy` default-deny dans chaque namespace d'environnement.
- `ResourceQuota`/`LimitRange` appliqués par défaut pour borner la consommation de ressources.
- `spec.source.repo` restreint à une liste blanche d'hôtes Git internes.
- Aucun secret de production n'est jamais injecté — les environnements de preview n'utilisent que des valeurs de test/mock.

Voir [`docs/dat-ephora-operator.md`](docs/dat-ephora-operator.md) pour l'architecture technique complète et [`docs/adrs-ephora-operator.md`](docs/adrs-ephora-operator.md) pour les décisions d'architecture (ADR).

## Développement

### Prérequis

| Outil | Version | Utilisé pour |
|---|---|---|
| [Go](https://go.dev/dl/) | >= 1.26 (voir `go.mod`) | compiler le manager, `make test` |
| GNU `make` | toute version | toutes les cibles du Makefile |
| [Docker](https://docs.docker.com/get-docker/) | récente | `make docker-build` |
| [kubectl](https://kubernetes.io/docs/tasks/tools/) | compatible avec votre cluster | `make install`, `make deploy` |
| [kind](https://kind.sigs.k8s.io/) (ou tout autre cluster) | récente | cluster local pour `make install` / `make run` |
| [kustomize](https://kubectl.docs.kubernetes.io/installation/kustomize/) | v5.x | `make install`, `make deploy` |
| [Helm](https://helm.sh/docs/intro/install/) | v3 | tester les charts applicatifs déployés par l'opérateur |
| git | toute version | cloner le dépôt ; aussi utilisé par le reconciler pour récupérer les charts |

`controller-gen` (v0.16.5) et `setup-envtest` sont installés automatiquement dans `./bin` par le Makefile.
`operator-sdk` n'est nécessaire que pour `make bundle` (packaging OLM) et est téléchargé à la demande — il n'existe pas de version Windows, utilisez WSL pour cette cible.

**Windows** : installez tout ce qui précède en une fois avec Chocolatey, depuis un PowerShell administrateur :

```powershell
powershell -ExecutionPolicy Bypass -File hack\install-dev-tools.ps1
```

Lancez ensuite les cibles `make` depuis Git Bash (le Makefile utilise `uname`/`pwd`).

**Linux/macOS** : installez les outils avec votre gestionnaire de paquets (ex. `brew install go make kubectl kind kustomize helm`).

### Premiers pas

```bash
git clone https://github.com/<org>/ephora-operator.git
cd ephora-operator
kind create cluster   # inutile si vous avez déjà un cluster dans votre contexte kube
make install          # installe les CRD
make run              # lance l'opérateur en local contre le contexte kube courant
make test             # lance les tests unitaires/d'intégration (envtest)
```

## Feuille de route

- [ ] Ingress public dynamique + intégration cert-manager (optionnel)
- [ ] Commentaire automatique sur la PR avec l'état/le lien de l'environnement
- [ ] Tableau de bord FinOps d'utilisation

## Licence

Apache 2.0
