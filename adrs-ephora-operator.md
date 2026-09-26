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
