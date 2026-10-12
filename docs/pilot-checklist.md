# Checklist du pilote — ephora-operator

Pour l'**équipe plateforme** qui met l'opérateur en service sur un vrai cluster et embarque une première équipe.
Références : [runbook](runbook.md) · [guide d'onboarding](onboarding.md) · [DAT](dat-ephora-operator.md).

---

## 0. Cadrer le pilote

- [ ] **Équipe pilote** choisie : motivée, avec un chart Helm simple, qui respecte la [checklist de compatibilité](onboarding.md#étape-1--vérifier-que-votre-chart-est-compatible).
- [ ] **Durée** fixée (ex. 4 semaines) et date du point de décision.
- [ ] **Critères de succès** convenus avec l'équipe :

| Critère | Mesure | Cible proposée |
|---|---|---|
| Adoption | % des PR de l'équipe avec un environnement | > 80 % |
| Rapidité | `preview_environment_provisioning_duration_seconds` (p95) | < 3 min |
| Fiabilité | Environnements `Failed` dus à la plateforme (hors erreurs de chart) ; redémarrages du pod opérateur | ≈ 0 |
| Nettoyage | Namespaces orphelins 1h après le TTL | 0 |
| Satisfaction | Retour qualitatif de l'équipe en fin de pilote | positif |
| Valeur | Délai de relecture des PR, avant / pendant le pilote | en baisse |

---

## 1. Prérequis du cluster

- [ ] Kubernetes ≥ 1.28.
- [ ] **CNI qui applique les NetworkPolicies** (Calico, Cilium…). Sans lui, l'isolation réseau est inactive **sans erreur visible**.
- [ ] Accès au registry `ghcr.io` depuis les nœuds (ou miroir interne de l'image).
- [ ] Le paquet GHCR `ephora-operator` est **public** (ou un `imagePullSecret` est prévu).
- [ ] Accès réseau sortant de l'opérateur vers le GitHub interne (clone des charts).
- [ ] Prometheus disponible (recommandé), avec son namespace et son ServiceAccount identifiés.
- [ ] Les noms de **groupes** des développeurs dans votre fournisseur d'identité (OIDC, LDAP…) sont connus.

---

## 2. Configurer l'overlay `config/prod`

Toutes les valeurs à adapter sont marquées `CHANGE-ME`.

| Fichier | Valeur | Exemple |
|---|---|---|
| `config/prod/kustomization.yaml` | Version de la release | `v0.1.2` |
| | Hôte Git autorisé (liste blanche) | `^https://github\.acme\.com/.+$` |
| | `--preview-ingress-namespaces` | `vpn-gateway,ingress-internal` |
| | `--viewer-groups` | `dev-checkout,dev-payment` |
| `config/prod/team-example.yaml` | Copier en `team-<nom>.yaml`, remplacer `example` et le groupe | `team-checkout.yaml` |

- [ ] Toutes les valeurs sont adaptées. Vérification, qui ne doit **rien** afficher :
  ```bash
  kustomize build config/prod | grep -n CHANGE-ME
  ```
- [ ] Le rendu est relu :
  ```bash
  kustomize build config/prod > /tmp/ephora-prod.yaml && less /tmp/ephora-prod.yaml
  ```

---

## 3. Vérifier la release avant de l'installer

- [ ] Signature de l'image (identité du workflow de release) :
  ```bash
  IMAGE=$(kustomize build config/prod | grep -m1 'image: ghcr.io' | awk '{print $2}')
  cosign verify "$IMAGE" \
    --certificate-identity-regexp '^https://github.com/ngatcheu/ephora-operator/.github/workflows/release.yml@refs/tags/v' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
  ```
- [ ] L'image est bien référencée **par digest** (`@sha256:…`), pas par tag.
- [ ] Notes de la release lues, et aucune CVE connue depuis sa publication (onglet *Security* du dépôt, ou `trivy image "$IMAGE"`).

---

## 4. Installer

- [ ] Déploiement :
  ```bash
  kustomize build config/prod | kubectl apply -f -
  kubectl -n ephora-operator-system rollout status deploy/ephora-operator-controller-manager
  ```
- [ ] **Identifiant Git** (si les dépôts sont privés) : Secret `ephora-operator-git-credentials`, créé hors git, avec un jeton **en lecture seule** ([runbook §7.3](runbook.md#73-configurer-laccès-aux-dépôts-privés)), puis redémarrage de l'opérateur.
- [ ] **Collecte des métriques** branchée ([runbook §4](runbook.md#brancher-prometheus)) et alertes configurées.
- [ ] Jeton CI de l'équipe pilote généré et transmis ([runbook §3](runbook.md#3-embarquer-une-équipe)).

---

## 5. Contrôles de sécurité

Chaque commande doit donner le résultat indiqué.

```bash
OP=system:serviceaccount:ephora-operator-system:ephora-operator-controller-manager
CI=system:serviceaccount:ephora-previews:ci-<équipe>

# L'opérateur n'a AUCUN droit sur les ressources applicatives ni sur les secrets
kubectl auth can-i create deployments -n default   --as=$OP     # no
kubectl auth can-i get secrets -n kube-system      --as=$OP     # no

# Le compte CI est limité aux PreviewEnvironment du namespace de gestion
kubectl auth can-i create previewenvironments -n ephora-previews --as=$CI   # yes
kubectl auth can-i get secrets -n ephora-previews                --as=$CI   # no
kubectl auth can-i list pods -n default                          --as=$CI   # no
```

- [ ] Résultats conformes.
- [ ] La CRD refuse un dépôt hors liste blanche :
  ```bash
  kubectl -n ephora-previews apply --dry-run=server -f - <<'EOF'
  apiVersion: ephora.io/v1alpha1
  kind: PreviewEnvironment
  metadata: {name: pr-1-check}
  spec:
    source: {repo: "https://github.com/evil/repo", chartPath: "charts/app", revision: "main"}
    prNumber: 1
    appName: check
    ttl: 1h
  EOF
  # attendu : erreur de validation sur spec.source.repo
  ```

---

## 6. Test de bout en bout (avant l'équipe pilote)

Avec un chart de test hébergé sur votre GitHub interne :

- [ ] Création → phase `Running`, pods prêts, NetworkPolicy, quota et `ephora-deployer` présents dans le namespace.
- [ ] **Isolation réseau** : un pod d'un namespace **non autorisé** ne joint pas l'environnement :
  ```bash
  kubectl run netcheck -n default --rm -it --restart=Never --image=busybox -- \
    wget -T 3 -qO- http://<service>.preview-pr-<n>-<app>.svc     # attendu : timeout
  ```
- [ ] Accès développeur : un membre du groupe `--viewer-groups` peut lire les logs et faire un `port-forward`, mais pas lire de Secret.
- [ ] Mise à jour : modifier `valuesOverride` → `helm upgrade` constaté.
- [ ] Suppression → namespace supprimé, métrique `preview_environment_cleanup_total{reason="deleted"}` incrémentée.
- [ ] Expiration : un environnement avec `ttl: 1h` disparaît seul après une heure.
- [ ] Métriques visibles dans Prometheus.

---

## 7. Embarquer l'équipe pilote

- [ ] Session d'intégration (1h) en suivant le [guide d'onboarding](onboarding.md).
- [ ] Workflow GitHub Actions ajouté au dépôt, première PR de test réussie.
- [ ] Canal de support ouvert et référent plateforme désigné.
- [ ] Point hebdomadaire : environnements `Failed` et leurs causes, retours de l'équipe, ajustements (quotas, types de ressources du rôle de déploiement).

---

## 8. Plan de retour arrière

Si le pilote doit être interrompu :

1. Retirer le workflow de preview du dépôt de l'équipe (plus aucune création).
2. Supprimer les environnements **pendant que l'opérateur tourne** :
   ```bash
   kubectl delete penv --all -n ephora-previews --wait=true
   ```
3. Désinstaller : `kustomize build config/prod | kubectl delete -f -`.
4. Révoquer le jeton Git et les jetons CI.

---

## 9. Décision de généralisation

- [ ] Critères de succès (§0) atteints ou écarts compris.
- [ ] Ajustements identifiés pendant le pilote intégrés (documentation, quotas, rôle de déploiement).
- [ ] Capacité validée pour le nombre d'équipes visé. L'opérateur tient 50 créations simultanées (p95 38 s sur kind avec 4 workers, voir [DAT §6](dat-ephora-operator.md#6-exigences-non-fonctionnelles)) ; reste à vérifier la capacité des **nœuds** à héberger les pods des environnements. Rejouer le test sur le cluster cible : `bash hack/loadtest.sh` (sur un cluster de test, le script refuse les contextes autres que kind).
- [ ] Planning d'embarquement des équipes suivantes.
- [ ] Bilan chiffré (adoption, délai de relecture, coûts évités) partagé, en interne ou dans une publication.
