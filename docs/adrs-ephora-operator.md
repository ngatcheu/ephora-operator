# ADRs — ephora-operator

## ADR-01 : Choix du framework — Operator SDK (plugin Helm)

### Statut
Accepté

### Contexte
L'opérateur doit instancier un chart Helm applicatif existant (propre à chaque dépôt) avec des valeurs spécifiques à une Pull Request, puis le détruire automatiquement après un délai (TTL) ou à la fermeture de la PR. Il n'y a pas de logique métier complexe à écrire : la tâche principale est l'orchestration paramétrée d'un chart déjà maintenu par les équipes applicatives.

### Décision
Utilisation d'**Operator SDK avec le plugin Helm** (`operator-sdk init --plugins=helm`) plutôt que Kubebuilder pur.

### Justification
- La réconciliation par défaut du plugin Helm applique directement `spec` comme `values` au chart référencé — pas de code Go à écrire pour le chemin nominal.
- Chaque équipe applicative garde la responsabilité de son chart ; l'opérateur reste agnostique du contenu applicatif, réduisant le couplage.
- Le gain de vélocité de développement est important : le scaffold de base couvre déjà 70% du besoin fonctionnel (déploiement paramétré).

### Conséquences
- **Positif** : développement rapide, faible dette de code, chart Helm existant réutilisé tel quel.
- **Négatif** : la logique additionnelle (gestion du TTL, création du namespace dédié, mise à jour du `.status`) doit être ajoutée via les hooks du plugin Helm (`watches.yaml`, réconciliation étendue) ou nécessite un passage partiel en Go si la complexité augmente. Point de vigilance à réévaluer si le TTL/finalizer devient trop complexe pour rester dans le plugin Helm.

### Mise en œuvre (v0.1)
Le point de vigilance s'est concrétisé : le **passage partiel en Go** prévu ci-dessus a été appliqué. Le projet reste scaffoldé avec Operator SDK, mais la réconciliation est un contrôleur Go (controller-runtime) au lieu du binaire générique `helm-operator`, pour trois raisons que le plugin ne couvre pas :
- le chart à déployer est **dynamique par instance** (dépôt et révision de chaque PR), et non un chart unique embarqué ;
- le TTL, le finalizer de nettoyage et le balayage des orphelins demandent une logique propre ;
- la création du namespace, de ses garde-fous et de l'identité de déploiement (ADR-05) précède chaque déploiement.

L'esprit de la décision est conservé : **tout déploiement passe par le SDK Helm** (install / upgrade / uninstall) et l'opérateur reste agnostique du contenu des charts. Il ne gère lui-même aucune ressource applicative.

---

## ADR-02 : Mécanisme de déclenchement — CI applique le CRD directement

### Statut
Accepté

### Contexte
La CI (GitHub Actions) doit déclencher la création de l'environnement à l'ouverture/mise à jour d'une PR, et sa destruction à la fermeture. Deux options : la CI applique/supprime directement le CRD via `kubectl`, ou l'opérateur expose un endpoint HTTP dédié.

### Décision
La CI effectue un `kubectl apply` / `kubectl delete` du CRD `PreviewEnvironment` directement, sans composant intermédiaire.

### Justification
- Cohérent avec le modèle déclaratif Kubernetes natif — pas de nouvelle surface d'API à sécuriser et maintenir.
- La CI dispose déjà d'un accès au cluster (pattern GitOps existant), donc pas de credential supplémentaire à provisionner.
- Réduit la surface d'attaque : pas d'endpoint HTTP exposé par l'opérateur à authentifier/rate-limiter.

### Conséquences
- **Positif** : simplicité, réutilisation de l'accès cluster existant de la CI, aucun composant réseau supplémentaire.
- **Négatif** : la CI doit avoir un RBAC scope limité au CRD `PreviewEnvironment` (namespace de gestion), à définir précisément pour éviter un accès trop large au cluster.

---

## ADR-03 : Exposition réseau — accès interne uniquement (V1)

### Statut
Accepté (à réévaluer)

### Contexte
Les environnements de preview doivent être accessibles par les équipes de review. Deux options : URL publique dynamique par PR (Ingress + DNS wildcard), ou accès interne (port-forward, VPN, Ingress interne statique).

### Décision
V1 : accès interne uniquement, pas de gestion DNS/TLS dynamique dans le contrôleur.

### Justification
- Réduit fortement la complexité initiale (pas de gestion de certificats, pas d'intégration DNS externe, pas de risque d'exposition publique d'environnements éphémères potentiellement peu durcis).
- Le besoin fonctionnel principal (review de code par l'équipe interne) est couvert sans URL publique.

### Conséquences
- **Positif** : scaffold plus simple, surface d'exposition réduite, cohérent avec une posture DevSecOps prudente par défaut.
- **Négatif** : expérience développeur moins fluide (pas de lien direct cliquable dans la PR) ; nécessite VPN ou port-forward pour accéder à l'environnement.
- **Révision prévue** : si l'adoption interne est forte, ajouter en V2 un Ingress dynamique avec certificat wildcard interne (cert-manager) sans exposition publique.

### Mise en œuvre (v0.1)
- Aucun Ingress ni Service exposé n'est créé par l'opérateur, et le rôle de déploiement des charts (ADR-05) **interdit les Ingress**.
- L'accès interne passe par `kubectl port-forward` (rôle lecture des développeurs, `--viewer-groups`) ou par une passerelle interne.
- La NetworkPolicy de chaque namespace n'accepte l'entrée que depuis le namespace lui-même et depuis les namespaces listés dans `--preview-ingress-namespaces` (passerelle VPN, ingress interne). Sans configuration, tout le cluster est accepté : à configurer en production.

---

## ADR-04 : Isolation — un namespace dédié par environnement

### Statut
Accepté

### Contexte
Chaque preview doit être isolée pour éviter les collisions de ressources entre PR (noms de Service, ConfigMaps, etc.) et permettre un nettoyage atomique.

### Décision
Chaque `PreviewEnvironment` provisionne son propre namespace (`preview-pr-<numéro>-<app>`), supprimé intégralement à l'expiration ou à la suppression du CRD.

### Justification
- Isolation forte par défaut (RBAC, NetworkPolicy, quotas appliqués au niveau namespace).
- Nettoyage atomique via suppression du namespace plutôt que suppression sélective de ressources — réduit le risque de fuite de ressources orphelines.

### Conséquences
- **Positif** : isolation et nettoyage simples et fiables.
- **Négatif** : prolifération de namespaces si le TTL n'est pas correctement appliqué — nécessite un mécanisme de finalizer robuste et un contrôle périodique de cohérence (reconciliation loop qui détecte les namespaces orphelins sans CRD associé).

---

## ADR-05 : Identité de déploiement — Helm exécuté sous un ServiceAccount propre à chaque namespace

### Statut
Accepté (v0.1)

### Contexte
Le chart déployé provient du dépôt applicatif, **à la révision de la PR** : son contenu est contrôlé par l'auteur de la PR et n'est pas relu au moment du déploiement. C'est donc une **entrée non fiable**. La liste blanche des dépôts (`spec.source.repo`) restreint l'origine, pas le contenu.

Dans une première version, Helm s'exécutait avec l'identité de l'opérateur, qui disposait de droits sur les ressources applicatives **dans tout le cluster**. Un chart malveillant pouvait alors :
- lire n'importe quel Secret du cluster via la fonction de template Helm `lookup` ;
- créer des ressources dans un autre namespace (`metadata.namespace` explicite), par exemple dans `kube-system`.

### Décision
Pour chaque namespace de preview, l'opérateur crée un ServiceAccount `ephora-deployer` (sans jeton monté) et un **RoleBinding local** vers le ClusterRole `ephora-operator-preview-deployer`. Helm s'exécute en **empruntant cette identité** (*impersonation* Kubernetes).

L'opérateur lui-même ne détient **aucun droit** sur les ressources applicatives. Il ne dispose que de :
- `impersonate` sur les ServiceAccounts nommés `ephora-deployer` ;
- `bind` sur le seul ClusterRole de déploiement, pour créer les RoleBindings sans en détenir les droits.

Le même mécanisme donne aux développeurs un accès en lecture et `port-forward` (`--viewer-groups`), lui aussi lié namespace par namespace, sans Secrets ni `exec`.

### Justification
- Le RBAC de Kubernetes devient la barrière de sécurité : chaque appel fait pendant le rendu et l'application du chart (y compris `lookup`) est autorisé contre les droits du namespace seul.
- L'architecture ne change pas (Helm, un namespace par environnement, ADR-04) : seul le périmètre des droits est réduit (moindre privilège).
- Comportement vérifié par des tests envtest, où l'API server applique réellement le RBAC : déploiement accepté dans le namespace, refusé dans `default`, `lookup` d'un Secret de `kube-system` refusé.

### Conséquences
- **Positif** : un chart compromis ne peut agir que dans son propre namespace éphémère. Les types autorisés sont listés explicitement dans `config/deployer/role.yaml` (sans Ingress, RBAC ni ressources cluster).
- **Négatif** : un chart qui a besoin d'un type de ressource non prévu échoue avec `forbidden` jusqu'à ce que le rôle soit élargi, après revue de sécurité. Le nom du ClusterRole doit rester aligné entre le code (marqueur `bind`), le flag `--deployer-cluster-role` et Kustomize.
- **Règle** : ne jamais redonner à l'opérateur des droits sur les ressources applicatives, ni lier le rôle de déploiement à l'échelle du cluster (inscrit dans `CLAUDE.md`).

---

## ADR-06 : Accès aux dépôts privés — jeton HTTPS unique, monté en fichier

### Statut
Accepté (v0.1)

### Contexte
Les dépôts applicatifs internes sont généralement privés. L'opérateur doit pouvoir les cloner, sans que l'identifiant fuie (logs, `status` des `PreviewEnvironment`, processus visibles, dépôt cloné) et sans élargir son RBAC.

Deux options :
1. un identifiant par équipe, référencé dans chaque `PreviewEnvironment` et stocké dans le namespace de l'équipe ;
2. un identifiant unique, en lecture seule, porté par l'opérateur.

### Décision
Option 2 en V1 : un **jeton HTTPS en lecture seule** (compte technique, droits *Contents : Read-only* sur les seuls dépôts intégrés), stocké dans le Secret `ephora-operator-git-credentials` et **monté en fichier** dans le pod de l'opérateur (volume optionnel).

Le jeton est transmis à git sous forme d'en-tête `Authorization` via les variables `GIT_CONFIG_*`, **limité à l'hôte du dépôt**, avec les redirections désactivées.

### Justification
- Aucun droit RBAC de lecture sur les Secrets n'est nécessaire (pas de lecture inter-namespace, contrairement à l'option 1).
- Le jeton n'apparaît ni dans la ligne de commande (`ps`), ni dans l'URL, ni dans `.git/config` du clone, ni dans la sortie d'erreur de git reprise dans la condition `Ready`. Vérifié par un test qui effectue un vrai `git clone` vers un serveur HTTP de test.
- Le fichier est relu à chaque clone : la rotation du Secret ne nécessite pas de redémarrage.

### Conséquences
- **Positif** : mise en place simple, surface d'attaque réduite, rotation sans interruption.
- **Négatif** : le compte technique doit pouvoir lire les dépôts de toutes les équipes intégrées. Pas de rafraîchissement automatique des jetons à durée courte (jetons d'installation GitHub App, valables une heure).
- **Révision prévue** : passer à des identifiants par équipe si le cloisonnement entre équipes devient une exigence.

---

## ADR-07 : Pod Security des namespaces de preview — `baseline` imposé, `restricted` signalé

### Statut
Accepté (v0.1.4)

### Contexte
Le chart déployé est une entrée non fiable (ADR-05). L'identité de déploiement limite **où** il peut agir, mais pas **ce que font ses pods** : sans contrôle, un chart peut lancer un pod privilégié, monter le système de fichiers du nœud (`hostPath`) ou utiliser son réseau (`hostNetwork`), et ainsi sortir de son namespace.

Jusqu'ici, les namespaces de preview héritaient du niveau Pod Security par défaut du cluster, ce qui donnait deux situations insatisfaisantes :
- cluster sans configuration : **aucun contrôle** ;
- RKE2 en profil CIS : `restricted` imposé, et **la plupart des charts applicatifs refusés** (pods en root, sans seccomp…), ce qui bloque l'adoption.

Options :
1. `restricted` imposé par défaut : sécurité maximale, mais chaque chart doit être durci avant le pilote ;
2. `baseline` imposé par défaut, `restricted` activable : bloque les vrais dangers (privilégié, accès à l'hôte) sans exiger de durcissement complet ;
3. laisser le niveau du cluster : comportement implicite et variable selon les clusters.

### Décision
Option 2. L'opérateur pose sur chaque namespace de preview les labels Pod Security Admission :
- `enforce` = niveau de `--preview-pod-security` : **`baseline` par défaut**, `restricted` sur option ;
- `warn` et `audit` = **`restricted`** dans tous les cas : chaque écart au niveau le plus strict est signalé (avertissements à l'équipe, journal d'audit), sans bloquer.

`privileged` n'est **pas** accepté par l'option : l'opérateur refuse de démarrer. Les labels sont rétablis à chaque réconciliation s'ils sont modifiés.

### Justification
- `baseline` couvre les scénarios d'évasion vers le nœud, le risque principal d'un chart malveillant ; `restricted` ajoute surtout de la défense en profondeur.
- Le pilote n'est pas bloqué par des charts encore non durcis, et les avertissements `restricted` donnent aux équipes la liste exacte de ce qu'il leur reste à corriger.
- Le comportement devient explicite et identique quel que soit le cluster.
- Vérifié par un test envtest où l'admission Pod Security de l'API server rejette réellement les pods.

### Conséquences
- **Positif** : plus aucun pod privilégié ni accès à l'hôte possible depuis une preview ; trajectoire claire vers `restricted`.
- **Négatif** : un pod refusé n'empêche pas le `helm install` de réussir — l'environnement est `Running` mais les pods manquent ; la cause apparaît dans les événements du namespace (`FailedCreate … violates PodSecurity`). L'opérateur a besoin du droit de mise à jour sur les namespaces.
- **Révision prévue** : passer le défaut à `restricted` quand les charts intégrés le respectent (suivi via les avertissements).
