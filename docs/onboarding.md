# Guide d'onboarding — environnements de preview

Ce guide s'adresse aux **équipes applicatives** qui veulent un environnement de preview automatique pour chaque Pull Request de leur dépôt.

Comptez **environ une heure** pour la première intégration.

---

## En bref

À chaque Pull Request, votre application est déployée automatiquement dans un namespace Kubernetes isolé, `preview-pr-<numéro>-<app>`, à la version exacte de la PR :

- **ouverture de la PR** → l'environnement est créé ;
- **nouveau commit** → l'environnement est mis à jour ;
- **PR fermée ou fusionnée** → l'environnement est supprimé ;
- **au plus tard à l'expiration du TTL** (48h par défaut, 7 jours maximum), même si la PR reste ouverte.

Vous n'avez rien à nettoyer : l'opérateur `ephora-operator` s'en charge.

---

## Étape 1 — Vérifier que votre chart est compatible

L'opérateur déploie **votre propre chart Helm**, depuis votre dépôt, à la révision de la PR. Il doit respecter les règles suivantes, sinon le déploiement échouera.

| ✔ | Exigence | Pourquoi |
|---|---|---|
| ☐ | Le chart est **dans le dépôt** de l'application (ex. `charts/app/`) | L'opérateur clone le dépôt à la révision de la PR |
| ☐ | Il se déploie avec **uniquement des `values`** (tag d'image, nombre de replicas…) | L'opérateur ne fait qu'un `helm install` / `helm upgrade` |
| ☐ | **Aucun secret de production**, ni dans le chart ni dans les values : uniquement des valeurs de test ou des mocks | Règle de sécurité, sans exception |
| ☐ | Uniquement des ressources **namespacées courantes** : Deployment, StatefulSet, Service, ConfigMap, Secret, Job, CronJob, HPA, PDB, PVC, ServiceAccount | Tout le reste est refusé (voir ci-dessous) |
| ☐ | **Pas d'Ingress** | Pas d'exposition réseau en V1 : accès interne uniquement |
| ☐ | **Pas de Role, RoleBinding, ClusterRole ni CRD** | Un chart ne peut pas se donner de droits |
| ☐ | Tout est créé dans **le namespace de la release** (pas de `metadata.namespace` en dur) | Le chart n'a aucun droit hors de son namespace |
| ☐ | Tient dans le **quota** : 2 CPU / 4 Gi demandés, 4 CPU / 8 Gi max, 20 pods | `ResourceQuota` du namespace |
| ☐ | **Pas d'accès à Internet** au démarrage (téléchargement, API externe…) | La sortie vers Internet est bloquée |
| ☐ | Les **images** sont accessibles depuis le cluster (registry interne) | Sinon les pods restent en `ImagePullBackOff` |
| ☐ | Pas de lien symbolique pointant hors du chart | Les liens sont désactivés au clonage |

> **Astuce** : si votre chart a besoin de dépendances (base de données, cache…), prévoyez une version légère activable par une value, par exemple `postgresql.enabled: true` avec un petit conteneur de test, plutôt qu'une dépendance à un service partagé.

---

## Étape 2 — Demander l'accès à l'équipe plateforme

Ouvrez une demande auprès de l'équipe plateforme en précisant :

- le **nom de votre dépôt** (il doit être sur le GitHub interne autorisé) ;
- le **nom de votre application** (`appName`) : minuscules, chiffres et tirets, 40 caractères maximum.

Vous recevrez :

| Élément | Usage |
|---|---|
| Le **namespace de gestion** (ex. `ephora-previews`) | Là où la CI crée les objets `PreviewEnvironment` |
| Un **kubeconfig CI**, à enregistrer dans le secret GitHub `PREVIEW_KUBECONFIG` de votre dépôt | Il ne permet **que** de créer et supprimer des `PreviewEnvironment` |
| Un **accès lecture** aux namespaces `preview-*` pour les développeurs | Consulter les pods, les logs, faire un `port-forward` |

---

## Étape 3 — Ajouter le template de l'environnement

Créez `preview-template.yaml` à la racine du dépôt. Les variables `${…}` sont remplacées par la CI.

```yaml
apiVersion: ephora.io/v1alpha1
kind: PreviewEnvironment
metadata:
  name: pr-${PR_NUMBER}-checkout-api       # ≤ 53 caractères
  namespace: ${PREVIEW_MGMT_NAMESPACE}
spec:
  source:
    repo: "https://github.internal/mon-org/checkout-api"
    chartPath: "charts/app"
    revision: "${REVISION}"                # commit de la PR
  prNumber: ${PR_NUMBER}
  appName: checkout-api
  ttl: 48h                                 # 168h maximum
  valuesOverride:
    image:
      tag: "${IMAGE_TAG}"
    replicaCount: 1
```

Remplacez `checkout-api`, le dépôt et `charts/app` par vos valeurs.

---

## Étape 4 — Ajouter le workflow GitHub Actions

Créez `.github/workflows/preview.yml`. L'image doit être **construite et poussée avant** la création de l'environnement : branchez ce job après votre job de build (`needs:`).

```yaml
name: Preview environment

on:
  pull_request:
    types: [opened, synchronize, reopened, closed]

jobs:
  # build:
  #   ... votre job existant qui construit et pousse l'image

  preview:
    # needs: build
    runs-on: ubuntu-latest
    env:
      PR_NUMBER: ${{ github.event.pull_request.number }}
      REVISION: ${{ github.event.pull_request.head.sha }}
      IMAGE_TAG: pr-${{ github.event.pull_request.number }}-${{ github.event.pull_request.head.sha }}
      PREVIEW_MGMT_NAMESPACE: ephora-previews
      KUBECONFIG: ${{ github.workspace }}/kubeconfig
    steps:
      - uses: actions/checkout@v7

      - name: Kubeconfig
        run: echo "${{ secrets.PREVIEW_KUBECONFIG }}" > "$KUBECONFIG"

      - name: Créer / mettre à jour l'environnement
        if: github.event.action != 'closed'
        run: envsubst < preview-template.yaml | kubectl apply -f -

      - name: Supprimer l'environnement
        if: github.event.action == 'closed'
        run: |
          kubectl delete previewenvironment "pr-${PR_NUMBER}-checkout-api" \
            -n "$PREVIEW_MGMT_NAMESPACE" --ignore-not-found
```

Ouvrez une PR de test : l'environnement doit apparaître en quelques secondes.

---

## Étape 5 — Suivre et utiliser l'environnement

### Voir l'état

```bash
kubectl get penv -n ephora-previews
```

```
NAME                   PHASE     PR     NAMESPACE                      EXPIRES                AGE
pr-1234-checkout-api   Running   1234   preview-pr-1234-checkout-api   2026-09-29T10:00:00Z   5m
```

| Phase | Signification |
|---|---|
| `Provisioning` | Déploiement en cours |
| `Running` | Prêt à tester |
| `Failed` | Erreur — voir [Dépannage](#dépannage) |
| `Expiring` / `Terminating` | Suppression en cours |

### Accéder à l'application

Il n'y a pas d'URL publique en V1. Passez par un `port-forward` :

```bash
kubectl port-forward -n preview-pr-1234-checkout-api svc/<nom-du-service> 8080:80
# puis http://localhost:8080
```

### Consulter les logs

```bash
kubectl get pods -n preview-pr-1234-checkout-api
kubectl logs -n preview-pr-1234-checkout-api deploy/<nom-du-deployment> -f
```

---

## Cycle de vie et TTL

- La date d'expiration est calculée **à la création** (`création + ttl`) et **ne change pas** si vous modifiez le `ttl` ensuite.
- À l'expiration, l'environnement est supprimé même si la PR est ouverte.
- **Pour en obtenir un nouveau**, poussez un commit sur la PR : la CI recrée l'environnement, avec un nouveau TTL.
- Les données (bases, fichiers) **ne survivent pas** à une suppression : prévoyez des données de test rechargées au démarrage.

---

## Dépannage

Commencez toujours par :

```bash
kubectl describe penv pr-1234-checkout-api -n ephora-previews
```

La section `Conditions` (champ `Reason` et `Message`) et les `Events` indiquent la cause.

| Symptôme / raison | Cause probable | Solution |
|---|---|---|
| `HelmDeployFailed` + `cloning …` | Dépôt inaccessible ou URL fausse | Vérifier `spec.source.repo` |
| `HelmDeployFailed` + `checking out revision` | Révision inexistante | Vérifier que le commit est poussé |
| `HelmDeployFailed` + `loading chart` | Mauvais `chartPath` ou chart invalide | `helm lint charts/app` en local |
| `HelmDeployFailed` + `forbidden` | Le chart crée une ressource interdite (Ingress, RBAC, ressource cluster, autre namespace) | Retirer ou désactiver cette ressource pour la preview |
| `HelmDeployFailed` + erreur de template | Value manquante ou erreur dans le chart | `helm template charts/app --set …` en local |
| `NamespaceReconcileFailed` + `already belongs to` | Un autre `PreviewEnvironment` utilise déjà le même numéro de PR et la même app | Supprimer le doublon |
| `Running` mais aucun pod | Quota dépassé | `kubectl get events -n preview-pr-…` → réduire les ressources demandées |
| Pods en `ImagePullBackOff` | Image absente ou tag faux | Vérifier que le build a poussé `IMAGE_TAG` |
| Pods en `CrashLoopBackOff` | L'application ne démarre pas | `kubectl logs …` ; vérifier les dépendances et l'accès réseau |
| Erreur à la création : `must be at most 53 characters` | Nom trop long | Raccourcir le nom dans le template |
| Erreur à la création : `ttl must not exceed 168h` | TTL trop long | 168h maximum |

Un environnement en `Failed` **réessaie automatiquement**, avec un délai croissant. Après correction, un nouveau commit relance immédiatement le déploiement.

---

## Bonnes pratiques

- **Gardez l'environnement léger** : 1 replica, petites ressources, dépendances simulées.
- **Préparez des données de test** chargées automatiquement au démarrage.
- **Testez votre chart en local** avant la première intégration : `helm lint` et `helm template`.
- **Ne mettez jamais de vrai secret** dans les values, même « temporairement ».

---

## Besoin d'aide ?

- Canal de support : `#preview-environments` *(à adapter)*
- Référents plateforme : *(à compléter)*
- Documentation technique : [README](../README.md) · [Architecture (DAT)](../dat-ephora-operator.md)
