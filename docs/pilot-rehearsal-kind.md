# Compte rendu — répétition du pilote sur kind

> Date : 2026-10-12 · Release : **v0.1.3** · Cluster : kind (Kubernetes 1.37, nœud unique), sur poste de développement (WSL)
> Déroulé : [checklist du pilote](pilot-checklist.md), avec une configuration de production réelle.

## Objectif

Dérouler **toute la checklist du pilote** avant la mise en service sur le cluster cible (RKE2), dans les conditions de la production :

- opérateur installé **depuis la release signée**, et non depuis les sources ;
- overlay `config/prod` rempli avec des valeurs réelles (aucun `CHANGE-ME`) ;
- une vraie application, sur un vrai dépôt GitHub, jouant le rôle de l'équipe pilote ;
- chaque action de la CI rejouée **avec l'identité du compte CI** (`kubectl --as`).

## Environnement

| Élément | Valeur |
|---|---|
| Opérateur | `ghcr.io/ngatcheu/ephora-operator@sha256:6793d326…` (v0.1.3, référencée par digest) |
| Liste blanche des dépôts | `^https://github\.com/ngatcheu/.+$` |
| `--preview-ingress-namespaces` | `vpn-gateway` (passerelle interne simulée) |
| `--viewer-groups` | `dev-pilote` (groupe de développeurs fictif) |
| Compte CI | `ephora-previews/ci-demo`, rôle `previewenvironment-editor` |
| Application pilote | [ngatcheu/ephora-demo-app](https://github.com/ngatcheu/ephora-demo-app) : nginx non-root, système de fichiers en lecture seule, page affichant le numéro et la révision de la PR |

L'overlay utilisé est une copie de `config/prod`, conservée **hors du dépôt public**.

## Résultats

| Phase | Contrôle | Résultat |
|---|---|---|
| **A. Installation** | Aucun `CHANGE-ME` dans le rendu | ✅ 0 |
| | Image référencée par digest | ✅ |
| | `cosign verify` : certificat du workflow de release, présence dans le registre de transparence | ✅ |
| | Installation sur cluster vierge, opérateur démarré | ✅ |
| **B. Moindre privilège** | Opérateur : créer un Deployment dans `default` | ✅ refusé |
| | Opérateur : lire les Secrets de `kube-system` | ✅ refusé |
| | CI : créer des `PreviewEnvironment` | ✅ autorisé |
| | CI : lire des Secrets, lister les pods de `default` | ✅ refusé |
| | Dépôt hors liste blanche (`github.com/helm/examples`) | ✅ rejeté par la CRD |
| **C. Création** | `PreviewEnvironment` de la PR #42 créé par le compte CI | ✅ `Running` en **3 s** |
| | Namespace : pod applicatif, `ephora-deployer`, `ephora-viewer`, NetworkPolicy, quota | ✅ |
| | Ressources du chart décomptées par le quota (50m CPU, 32 Mio) | ✅ |
| **D. Accès développeur** (`dev-pilote`) | Lister les pods, `port-forward` vers la page | ✅ autorisé — page « Preview PR #42 » affichée |
| | Lire les Secrets, `exec` | ✅ refusé |
| **E. Isolation réseau** | Depuis `default` (non autorisé) | ✅ connexion refusée |
| | Depuis `vpn-gateway` (autorisé) | ✅ page servie |
| **F. Mise à jour** | Nouveau commit sur la PR → nouvelle révision appliquée | ✅ `observedGeneration` 2/2, révision `751f30f` |
| **G. Suppression** | Fermeture de la PR (suppression par le compte CI) | ✅ release désinstallée et namespace supprimé avant le retour de `kubectl delete` |

Capacité (mesurée séparément avec `hack/loadtest.sh` sur le même poste) : 50 environnements créés d'un coup, tous `Running` en 41 s, p95 38 s, mémoire de l'opérateur 95 Mio au pic.

## Non couvert par cette répétition

| Point | Raison | Couverture actuelle |
|---|---|---|
| Expiration par TTL | Nécessite au moins une heure | Tests envtest ; à constater pendant le pilote |
| Métriques dans Prometheus | Pas de Prometheus sur kind | Accès HTTPS authentifié vérifié séparément (`401` sans jeton, métriques servies avec jeton) |
| CI GitHub Actions de bout en bout | Les runners GitHub ne peuvent pas joindre un kind local | Commandes de la CI rejouées avec l'identité du compte CI (`--as`) |
| Pod Security Admission | kind n'applique pas le profil CIS de RKE2 | Traité ensuite par l'ADR-07 (niveau posé par namespace, test envtest) ; à confirmer sur RKE2 |
| Dépôts privés | Dépôt de démonstration public | Tests unitaires (clone authentifié réel vers un serveur de test) |

## Constats et corrections

- **cosign absent des outils de développement** : ajouté à `hack/install-dev-tools.sh`, avec vérification de l'empreinte.
- **`kubectl auth can-i create pods/portforward` donne un faux négatif** : cette syntaxe désigne un *pod nommé* `portforward`. La forme correcte est `kubectl auth can-i create pods --subresource=portforward`. La documentation n'était pas concernée.
- **buildx dans WSL** : le lien fourni par Docker Desktop se casse au redémarrage ; `hack/install-dev-tools.sh` installe désormais buildx dans `~/.docker/cli-plugins`.

## Passage sur RKE2

Reprendre les mêmes phases avec les valeurs du cluster cible, en commençant par :

1. **Pod Security** : décision prise depuis cette répétition ([ADR-07](adrs-ephora-operator.md)) — l'opérateur impose `baseline` sur chaque namespace de preview (`restricted` sur option) et signale les écarts à `restricted`. Sur RKE2 en profil CIS, vérifier que ce niveau posé par namespace est bien appliqué, et que les charts de l'équipe pilote passent (`kubectl get events … | grep PodSecurity`).
2. **CNI** : confirmer que Canal (ou Cilium) applique les NetworkPolicies — rejouer la phase E.
3. **Supervision** : avec Rancher Monitoring, activer le `ServiceMonitor` ([runbook §4](runbook.md#brancher-prometheus)).
4. **CI réelle** : générer le kubeconfig du compte CI ([runbook §3](runbook.md#3-embarquer-une-équipe)) et le placer dans le secret `PREVIEW_KUBECONFIG` du dépôt pilote.
