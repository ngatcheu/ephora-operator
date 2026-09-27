# Runbook d'exploitation — ephora-operator

Ce runbook s'adresse à l'**équipe plateforme / SRE** qui installe, surveille et dépanne l'opérateur.
Pour l'intégration des dépôts applicatifs, voir le [guide d'onboarding](onboarding.md).

---

## Sommaire

1. [Vue d'ensemble](#1-vue-densemble)
2. [Installation et mise à jour](#2-installation-et-mise-à-jour)
3. [Embarquer une équipe](#3-embarquer-une-équipe)
4. [Surveillance](#4-surveillance)
5. [Diagnostic](#5-diagnostic)
6. [Procédures d'incident](#6-procédures-dincident)
7. [Opérations courantes](#7-opérations-courantes)
8. [Limites connues](#8-limites-connues)

---

## 1. Vue d'ensemble

### Composants déployés (`make deploy`)

| Ressource | Nom | Rôle |
|---|---|---|
| Namespace | `ephora-operator-system` | Héberge l'opérateur |
| Deployment | `ephora-operator-controller-manager` | L'opérateur (1 replica, élection de leader activée) |
| ServiceAccount | `ephora-operator-controller-manager` | Identité de l'opérateur |
| ClusterRole | `ephora-operator-manager-role` | Droits de l'opérateur (généré depuis le code) |
| ClusterRole | `ephora-operator-preview-deployer` | Droits accordés aux charts, **lié par namespace** uniquement |
| ClusterRole | `ephora-operator-preview-viewer` | Lecture + `port-forward` des développeurs, **lié par namespace** aux `--viewer-groups` |
| ClusterRole | `ephora-operator-previewenvironment-editor-role` | À lier aux comptes CI des équipes |
| ClusterRole | `ephora-operator-metrics-reader` | À lier au compte de Prometheus |
| Service | `ephora-operator-controller-manager-metrics-service` | Métriques HTTPS sur le port 8443 |
| NetworkPolicy | `ephora-operator-allow-metrics-traffic` | Seuls les namespaces labellisés `metrics: enabled` atteignent le port 8443 |
| CRD | `previewenvironments.ephora.io` (`penv`) | L'API |

### Ce que l'opérateur crée dans chaque namespace `preview-pr-<n>-<app>`

| Ressource | Nom |
|---|---|
| NetworkPolicy (default-deny, sortie limitée au DNS et au cluster) | `ephora-default` |
| ResourceQuota (2 CPU / 4 Gi demandés, 4 CPU / 8 Gi max, 20 pods) | `ephora-default` |
| LimitRange (valeurs par défaut par conteneur) | `ephora-default` |
| ServiceAccount utilisé par Helm (sans jeton monté) | `ephora-deployer` |
| RoleBinding → `ephora-operator-preview-deployer` | `ephora-deployer` |
| RoleBinding → `ephora-operator-preview-viewer` (si `--viewer-groups` est défini) | `ephora-viewer` |
| Release Helm (stockée en Secret) | nom du `PreviewEnvironment` |

Tous portent les labels `ephora.io/managed-by=ephora-operator`, `ephora.io/owner=<nom>` et `ephora.io/owner-namespace=<namespace de gestion>`.

### Flux normal

```
CI apply penv ─▶ finalizer ─▶ namespace + garde-fous + identité ─▶ git clone ─▶ helm install ─▶ Running
                                                                                         │
      CI delete penv / TTL expiré ─▶ helm uninstall ─▶ suppression du namespace ─▶ retrait du finalizer
```

---

## 2. Installation et mise à jour

### Prérequis

- Kubernetes ≥ 1.28 (règles de validation CEL).
- Un CNI qui applique les NetworkPolicies (Calico, Cilium…). Sans lui, l'isolation réseau est **silencieusement inactive**.
- Un registry accessible pour l'image de l'opérateur.
- La liste blanche `spec.source.repo` adaptée à votre GitHub interne (`api/v1alpha1/previewenvironment_types.go`, puis `make manifests`).

### Installer

```bash
make docker-build docker-push IMG=<registry>/ephora-operator:v0.1.0
make deploy IMG=<registry>/ephora-operator:v0.1.0

kubectl -n ephora-operator-system rollout status deploy/ephora-operator-controller-manager
kubectl create namespace ephora-previews          # namespace de gestion des PreviewEnvironment
```

### Mettre à jour

```bash
make deploy IMG=<registry>/ephora-operator:v0.2.0
kubectl -n ephora-operator-system rollout status deploy/ephora-operator-controller-manager
```

`make deploy` applique aussi la CRD. Les environnements existants sont repris automatiquement au démarrage.

**Retour arrière** : relancer `make deploy` avec l'image précédente. Si la nouvelle version a ajouté un champ à la CRD, l'ancienne version l'ignore sans erreur.

### Désinstaller

> ⚠️ **Ordre impératif.** Supprimer d'abord tous les `PreviewEnvironment`, **pendant que l'opérateur tourne** : c'est lui qui retire les finalizers. Un `make undeploy` direct supprime l'opérateur puis la CRD, et les objets restent bloqués en suppression.

```bash
kubectl delete penv --all -A --wait=true
kubectl get ns -l ephora.io/managed-by=ephora-operator     # doit être vide
make undeploy
```

---

## 3. Embarquer une équipe

Pour chaque équipe, créer un compte CI limité aux `PreviewEnvironment` du namespace de gestion :

```bash
TEAM=checkout
kubectl -n ephora-previews create serviceaccount ci-$TEAM
kubectl -n ephora-previews create rolebinding ci-$TEAM \
  --clusterrole=ephora-operator-previewenvironment-editor-role \
  --serviceaccount=ephora-previews:ci-$TEAM

# Jeton à durée limitée (à renouveler, voir §7)
TOKEN=$(kubectl -n ephora-previews create token ci-$TEAM --duration=720h)
```

Construire un kubeconfig avec ce jeton, l'URL de l'API et le CA du cluster, puis le transmettre à l'équipe pour le secret GitHub `PREVIEW_KUBECONFIG`.

Pour que les **développeurs** de l'équipe puissent consulter leurs environnements (pods, logs, `port-forward`), ajouter leur groupe à `--viewer-groups` (voir [§7.5](#75-changer-les-paramètres-de-lopérateur)) : l'opérateur crée alors un RoleBinding en lecture dans **chaque** namespace de preview. Ce rôle exclut les Secrets et `pods/exec`. Pour lister les `PreviewEnvironment`, lier aussi le groupe au rôle `ephora-operator-previewenvironment-viewer-role` dans le namespace de gestion :

```bash
kubectl -n ephora-previews create rolebinding devs-$TEAM \
  --clusterrole=ephora-operator-previewenvironment-viewer-role --group=<groupe-devs>
```

Vérifier que le compte CI n'a **que** les droits attendus :

```bash
kubectl auth can-i create previewenvironments -n ephora-previews --as=system:serviceaccount:ephora-previews:ci-$TEAM   # yes
kubectl auth can-i get secrets -n ephora-previews           --as=system:serviceaccount:ephora-previews:ci-$TEAM   # no
kubectl auth can-i create deployments -n default            --as=system:serviceaccount:ephora-previews:ci-$TEAM   # no
```

---

## 4. Surveillance

### Santé

| Point | Adresse |
|---|---|
| Liveness | `:8081/healthz` |
| Readiness | `:8081/readyz` |
| Métriques Prometheus | `https://:8443/metrics` (Service `ephora-operator-controller-manager-metrics-service`) |

### Brancher Prometheus

Les métriques sont en **HTTPS avec un certificat auto-signé** et **réservées aux clients autorisés** : le jeton du client est vérifié (TokenReview), puis son droit `GET /metrics` (SubjectAccessReview).

1. **Autoriser Prometheus** à lire les métriques :
   ```bash
   kubectl create clusterrolebinding ephora-operator-metrics-reader \
     --clusterrole=ephora-operator-metrics-reader \
     --serviceaccount=<namespace-prometheus>:<serviceaccount-prometheus>
   ```
2. **Ouvrir le réseau** depuis le namespace de Prometheus :
   ```bash
   kubectl label namespace <namespace-prometheus> metrics=enabled
   ```
3. **Déclarer la collecte** :
   - avec le Prometheus Operator : décommenter `- ../prometheus` dans `config/default/kustomization.yaml`, puis `make deploy`. Le `ServiceMonitor` fourni utilise le jeton du ServiceAccount de Prometheus et `insecureSkipVerify` (certificat auto-signé) ;
   - sinon : configurer un job de collecte vers le Service, en `https`, avec le jeton du ServiceAccount de Prometheus.

Vérification manuelle :

```bash
kubectl -n ephora-operator-system port-forward svc/ephora-operator-controller-manager-metrics-service 8443 &
TOKEN=$(kubectl -n <namespace-prometheus> create token <serviceaccount-prometheus>)
curl -sk -H "Authorization: Bearer $TOKEN" https://localhost:8443/metrics | grep preview_
```

Sans jeton, ou avec un jeton non autorisé, la réponse est `401` ou `403`.

> Pour la production, remplacer le certificat auto-signé par un certificat cert-manager et retirer `insecureSkipVerify` (voir les commentaires de `config/prometheus/monitor.yaml`).

### Métriques clés

| Métrique | Lecture |
|---|---|
| `preview_environments_active` | Environnements `Running` (recalculé à chaque balayage) |
| `preview_environment_provisioning_duration_seconds` | Délai création → premier `Running` |
| `preview_environment_cleanup_total{reason}` | Nettoyages : `deleted`, `ttl_expired`, `orphan_sweep` |
| `controller_runtime_reconcile_errors_total{controller="previewenvironment"}` | Erreurs de réconciliation |
| `workqueue_depth{name="previewenvironment"}` | File d'attente de l'opérateur |

### Alertes recommandées

| Alerte | Expression PromQL | Sévérité |
|---|---|---|
| Opérateur absent | `absent(up{job="ephora-operator"} == 1)` | critique |
| Provisionnement lent (NFR : < 3 min) | `histogram_quantile(0.95, sum by (le) (rate(preview_environment_provisioning_duration_seconds_bucket[1h]))) > 180` | avertissement |
| Nettoyages par balayage (un finalizer a expiré) | `increase(preview_environment_cleanup_total{reason="orphan_sweep"}[1h]) > 0` | avertissement |
| Trop d'environnements (NFR : 50) | `preview_environments_active > 50` | avertissement |
| Erreurs de réconciliation en hausse | `rate(controller_runtime_reconcile_errors_total{controller="previewenvironment"}[15m]) > 0.1` | avertissement |

> Il n'existe pas encore de métrique par phase : pour compter les environnements `Failed`, voir [§5](#5-diagnostic).

---

## 5. Diagnostic

### Vue d'ensemble

```bash
kubectl get penv -A                                               # tous les environnements
kubectl get penv -A | grep -E 'Failed|Terminating'                 # ceux qui posent problème
kubectl get ns -l ephora.io/managed-by=ephora-operator             # namespaces gérés
```

### Un environnement précis

```bash
kubectl describe penv <nom> -n ephora-previews                     # conditions + événements
kubectl get penv <nom> -n ephora-previews -o jsonpath='{.status}' | jq
kubectl get all,events -n preview-pr-<n>-<app>
helm list -n preview-pr-<n>-<app>                                  # avec un compte admin
helm history <nom> -n preview-pr-<n>-<app>
```

### L'opérateur

```bash
kubectl -n ephora-operator-system logs deploy/ephora-operator-controller-manager --since=1h
kubectl -n ephora-operator-system logs deploy/ephora-operator-controller-manager --since=1h | grep '"level":"error"'
```

Chaque erreur de réconciliation apparaît une fois dans les logs (`Reconciler error`), avec le nom de l'objet, et en événement `Warning` sur le `PreviewEnvironment`.

---

## 6. Procédures d'incident

### 6.1 Un environnement est en `Failed`

1. `kubectl describe penv <nom>` → lire `Reason` et `Message` de la condition `Ready`.
2. Les erreurs liées **au chart ou au dépôt** (`HelmDeployFailed`) relèvent de l'équipe applicative : les renvoyer vers le [tableau de dépannage de l'onboarding](onboarding.md#dépannage).
3. Les erreurs **côté plateforme** :

| Raison | Cause | Action |
|---|---|---|
| `DeployerIdentityReconcileFailed` | ClusterRole `ephora-operator-preview-deployer` absent, ou l'opérateur n'a pas le droit `bind` dessus | `kubectl get clusterrole ephora-operator-preview-deployer` ; redéployer (`make deploy`). Si le rôle a été renommé, aligner `--deployer-cluster-role` et le marqueur `bind` |
| `HelmDeployFailed` + `forbidden … ephora-deployer` | Le chart utilise un type de ressource absent du rôle de déploiement | Voir [§7.4](#74-autoriser-un-nouveau-type-de-ressource) |
| `NamespaceReconcileFailed` + `not managed by ephora-operator` | Un namespace `preview-pr-…` existe déjà, créé hors opérateur | Vérifier qu'il est inutilisé, puis le supprimer |
| `NamespaceReconcileFailed` + `already belongs to` | Deux `PreviewEnvironment` pour la même PR et la même app | Supprimer le doublon |

L'opérateur réessaie automatiquement avec un délai croissant (jusqu'à environ 16 min). Pour forcer une nouvelle tentative immédiate, deux possibilités :

- **un nouveau commit sur la PR** : il modifie le `spec` et déclenche une réconciliation de cet environnement ;
- **redémarrer l'opérateur**, qui relance tous les environnements :
  ```bash
  kubectl -n ephora-operator-system rollout restart deploy/ephora-operator-controller-manager
  ```

Modifier une annotation ou un label **ne suffit pas** : seuls les changements du `spec` déclenchent une réconciliation.

### 6.2 Un `PreviewEnvironment` reste bloqué en suppression

**Symptôme** : `kubectl delete penv` ne rend pas la main, l'objet a un `deletionTimestamp`.

1. **L'opérateur tourne-t-il ?** Sans lui, personne ne retire le finalizer.
   ```bash
   kubectl -n ephora-operator-system get pods
   ```
   S'il est arrêté, le redémarrer : le nettoyage reprend tout seul.
2. **S'il tourne**, les logs doivent montrer `waiting for namespace to terminate`. C'est normal pendant la suppression du namespace. Au-delà de `--cleanup-timeout` (10 min par défaut), l'opérateur libère le finalizer de lui-même, et le balayage des orphelins reprendra la suppression du namespace.
3. **En dernier recours seulement**, et après avoir vérifié les deux points précédents, retirer le finalizer à la main :
   ```bash
   kubectl patch penv <nom> -n ephora-previews --type json \
     -p '[{"op":"remove","path":"/metadata/finalizers"}]'
   kubectl delete ns preview-pr-<n>-<app> --wait=false      # nettoyer le namespace soi-même
   ```
   > Ne jamais retirer le finalizer de la CRD ni du code pour débloquer une situation : on perdrait la garantie de nettoyage pour tous les environnements.

### 6.3 Un namespace de preview reste en `Terminating`

La cause est presque toujours une ressource du namespace qui porte elle-même un finalizer (PVC protégé, ressource personnalisée dont le contrôleur est absent…).

```bash
kubectl get ns preview-pr-<n>-<app> -o jsonpath='{.status.conditions}' | jq
kubectl api-resources --verbs=list --namespaced -o name \
  | xargs -n1 kubectl get -n preview-pr-<n>-<app> --ignore-not-found --show-kind
```

Identifier la ressource bloquante et traiter son finalizer. L'opérateur n'a rien à faire ici : il a déjà libéré le `PreviewEnvironment` après le délai maximal.

### 6.4 Namespaces orphelins

Un namespace `ephora.io/managed-by=ephora-operator` sans `PreviewEnvironment` correspondant est supprimé automatiquement par le balayage (au démarrage, puis toutes les 30 min). Une hausse de `preview_environment_cleanup_total{reason="orphan_sweep"}` signale des nettoyages qui ont dû passer par ce filet de sécurité : regarder les logs de l'opérateur autour de ces suppressions.

### 6.5 Release Helm bloquée (`pending-install`, `pending-upgrade`, `failed`)

L'opérateur **réinstalle automatiquement** une release interrompue ou jamais déployée : aucune action n'est normalement nécessaire. Si le problème persiste :

```bash
helm history <nom> -n preview-pr-<n>-<app>
helm uninstall <nom> -n preview-pr-<n>-<app>       # l'opérateur réinstalle à la réconciliation suivante
```

### 6.6 Quota dépassé

**Symptôme** : l'environnement est `Running` (le `helm install` a réussi) mais des pods manquent.

```bash
kubectl get events -n preview-pr-<n>-<app> --field-selector reason=FailedCreate
kubectl describe resourcequota ephora-default -n preview-pr-<n>-<app>
```

En premier lieu, demander à l'équipe de réduire ses ressources pour la preview. Les quotas sont aujourd'hui **fixés dans le code** (`internal/controller/namespace.go`) : les augmenter demande une nouvelle version de l'opérateur, et vaut pour tous les environnements.

### 6.7 Incident de sécurité (chart suspect)

1. Supprimer l'environnement : `kubectl delete penv <nom> -n ephora-previews`.
2. Révoquer le compte CI de l'équipe si le dépôt est compromis :
   ```bash
   kubectl -n ephora-previews delete rolebinding ci-<team>
   kubectl -n ephora-previews delete serviceaccount ci-<team>     # invalide tous ses jetons
   ```
3. Récupérer le chart à la révision concernée et l'analyser. Pour rappel, ce qu'un chart **ne peut pas** faire : agir hors de son namespace, lire des secrets ailleurs (`lookup`), créer des ressources cluster, de l'Ingress ou du RBAC, accéder à Internet, lire les fichiers de l'opérateur via un lien symbolique.
4. Vérifier les logs d'audit de l'API server pour l'utilisateur `system:serviceaccount:preview-pr-<n>-<app>:ephora-deployer`.

### 6.8 Urgence : tout supprimer (coût, incident)

```bash
kubectl delete penv --all -n ephora-previews --wait=false
kubectl get ns -l ephora.io/managed-by=ephora-operator -w
```

Pour empêcher toute recréation pendant l'incident, retirer temporairement les rolebindings `ci-*` du namespace de gestion.

---

## 7. Opérations courantes

### 7.1 Lister les environnements par ancienneté

```bash
kubectl get penv -A --sort-by=.metadata.creationTimestamp
```

### 7.2 Renouveler un jeton CI

```bash
kubectl -n ephora-previews create token ci-<team> --duration=720h
```

Mettre à jour le secret `PREVIEW_KUBECONFIG` du dépôt de l'équipe. Planifier ce renouvellement avant l'expiration (30 jours dans l'exemple).

### 7.3 Modifier la liste blanche des dépôts

1. Modifier le motif `+kubebuilder:validation:Pattern` de `Repo` dans `api/v1alpha1/previewenvironment_types.go`.
2. `make manifests`, relire la CRD générée, commiter.
3. `make deploy IMG=…`.

Les objets existants ne sont pas revalidés, mais toute modification ultérieure de leur `spec` le sera.

### 7.4 Autoriser un nouveau type de ressource

Quand un chart légitime a besoin d'un type absent du rôle de déploiement :

1. **Évaluer le risque** : le type permet-il d'agir hors du namespace, d'élever des droits ou d'exposer l'environnement ? Ingress, Role/RoleBinding et ressources cluster sont **exclus volontairement** (ADR-03 et modèle de sécurité).
2. Ajouter la règle dans `config/deployer/role.yaml`, faire relire la modification.
3. `make deploy IMG=…`. Le changement s'applique immédiatement à tous les namespaces : ils sont liés au même ClusterRole.

### 7.5 Changer les paramètres de l'opérateur

Modifier les `args` dans `config/manager/manager.yaml`, puis `make deploy`.

| Flag | Défaut |
|---|---|
| `--orphan-sweep-interval` | `30m` |
| `--cleanup-timeout` | `10m` |
| `--deployer-cluster-role` | `ephora-operator-preview-deployer` |
| `--viewer-groups` | *(vide : aucun accès développeur)* — ex. `--viewer-groups=dev-checkout,dev-payment` |
| `--viewer-cluster-role` | `ephora-operator-preview-viewer` |
| `--metrics-secure`, `--metrics-require-rbac` | activés par `make deploy` (ne pas les retirer) |
| `--leader-elect` | activé dans le déploiement |

Un changement de `--viewer-groups` s'applique à chaque namespace au plus tard 10 min après le redémarrage (réconciliation périodique). Les noms de groupe dépendent de votre fournisseur d'identité (OIDC, LDAP…).

---

## 8. Limites connues

| Limite | Conséquence | Contournement |
|---|---|---|
| Le `ServiceMonitor` n'est pas activé par défaut (il exige le Prometheus Operator) | Collecte à déclarer | Voir [§4](#brancher-prometheus) |
| Certificat des métriques auto-signé | `insecureSkipVerify` côté Prometheus | cert-manager (à venir) |
| Pas d'authentification Git | Seuls les dépôts clonables sans identifiants fonctionnent | À venir |
| Entrée de la NetworkPolicy ouverte à tous les namespaces | Un pod de n'importe quel namespace peut joindre un environnement | Restreindre à la passerelle interne (à venir) |
| Quotas fixés dans le code | Pas d'ajustement par équipe | Nouvelle version de l'opérateur |
| Pas de métrique par phase | Les `Failed` ne sont pas visibles dans Prometheus | `kubectl get penv -A` |
