# PUSH — Push Unaware Serving Host

> Brief de lancement du projet. À placer à la racine du dépôt (ou à renommer `CLAUDE.md`) pour démarrer avec Claude Code.

## 1. En une phrase

Un petit serveur **générique, auto-hébergé, qui ne sait rien** : n'importe quelle PWA lui dit « envoie un Web Push à cet appareil à telle heure, avec cet identifiant opaque », et lui s'exécute au bon moment sans connaître ni le contenu, ni le but, ni l'utilisateur.

Nom : **PUSH** = *Push Unaware Serving Host* (acronyme récursif façon PHP) : un hôte qui sert des choses dont il ignore tout. Le dernier mot (« Host ») désigne bien le serveur lui-même ; la cible principale reste les téléphones et tablettes. (Un premier nom, « Push Unaware Serving Handsets », a été écarté : en anglais il se lisait comme des smartphones ignorants qui servent.)

## 2. Pourquoi ce projet existe

- Raphaël a une app Android (APK), **Phms** (gestion de stock d'armoire à pharmacie, rappels de prise de médicaments), dont le cœur repose sur les **notifications**.
- Il veut la proposer aux utilisateurs iPhone **sans payer les 99 $/an** du compte développeur Apple, via une **PWA**.
- Contraintes iOS établies :
  - App native avec Apple ID gratuit : notifs locales OK, **push distants (APNs) impossibles**, et signature à renouveler tous les 7 jours.
  - **PWA : le Web Push marche gratuitement** (iOS 16.4+, PWA obligatoirement ajoutée à l'écran d'accueil, permission demandée suite à une action utilisateur).
  - Une PWA **ne peut pas planifier de notifs locales fiables** quand elle est fermée → il faut un serveur qui déclenche les push à l'heure.
- Phms a deux modes : **100 % local** (données sur le téléphone, pas de serveur) et **foyer** (synchro via PocketBase, déjà déployé). Le mode local ne doit pas être forcé de passer par un compte/serveur de synchro → d'où ce mini-serveur dédié aux push.
- Raphaël veut que PUSH soit **un projet totalement indépendant de Phms** (propre dépôt, propre cycle de vie), réutilisable pour d'autres PWA. Phms n'est qu'un client parmi d'autres, rattaché plus tard.

## 3. Principe de fonctionnement

1. La PWA s'abonne au Web Push (service worker + clés VAPID publiques du serveur).
2. Pour chaque rappel, la PWA appelle l'API de PUSH avec : **abonnement push**, **planification** (heure unique ou règle récurrente), **identifiant opaque** (UUID généré par la PWA).
3. À l'heure prévue, le serveur envoie un push dont le payload contient **uniquement l'identifiant**.
4. Le service worker lit l'identifiant, retrouve le texte du rappel **en local** (IndexedDB) et affiche la notification.

→ Le contenu du rappel **ne quitte jamais le téléphone**. Pas de chiffrement de message à gérer, pas de clé à distribuer. Zéro donnée de santé côté serveur.

## 4. Données stockées côté serveur (et rien d'autre)

| Donnée | Détail |
|---|---|
| Application | Clé d'API / identifiant d'app (voir §6) |
| Abonnement push | endpoint + clés `p256dh` / `auth` fournies par le navigateur |
| Identifiant opaque | UUID de la PWA, renvoyé tel quel dans le push |
| Planification | Rappel unique : date/heure UTC. Récurrent : règle simple (ex. « tous les jours à 8h ») **+ fuseau IANA de l'appareil** pour que le changement d'heure ne décale rien |
| Jeton de gestion | Généré à la création, requis pour modifier/supprimer. Pas de compte utilisateur |

## 5. API (3 routes minimum)

- **Créer un rappel** : abonnement + identifiant + planification (+ clé d'app) → renvoie le jeton de gestion.
- **Supprimer un rappel** : identifiant + jeton de gestion.
- **Lister les rappels d'un abonnement** : abonnement + jeton, pour que la PWA puisse resynchroniser.

À prévoir en plus : route d'exposition de la clé publique VAPID, et un endpoint de santé.

## 6. Multi-applications (décidé en cours de discussion)

PUSH doit servir plusieurs PWA, pas seulement Phms :
- Notion d'**application** avec **clé d'API** ; les rappels de chaque app sont isolés des autres.
- Permet de limiter les abus **par application** (quotas).
- **Une seule paire de clés VAPID pour tout le serveur.** Attention : un abonnement push est lié à la clé publique VAPID avec laquelle il a été créé → si on la change, **tous les abonnements meurent**. Sauvegarde précieuse de cette clé obligatoire.
- Aucune logique ni mention de Phms dans le code ou l'API : garder l'API propre et générique.

## 7. Cron / envoi

- Passage **toutes les minutes**.
- Sélectionne les rappels dont l'heure est atteinte, envoie le push (protocole Web Push standard, VAPID, payload = identifiant seul).
- Rappel unique : supprimé après envoi. Récurrent : prochaine occurrence recalculée (en tenant compte du fuseau).
- Si le service push répond que l'abonnement n'existe plus (**404 / 410**) : supprimer tous les rappels liés à cet abonnement.
- Prévoir retries raisonnables et une gestion du rattrapage si le serveur a été arrêté (que faire des rappels « ratés » : envoyer quand même ou ignorer ? à trancher).

## 8. Côté PWA cliente (à documenter pour les intégrateurs)

- Le service worker, à chaque push : lit l'identifiant → cherche le texte dans IndexedDB → affiche la notif.
- **Obligatoire sur iOS : toujours afficher une notification visible.** Si l'identifiant est introuvable en local, afficher un message générique (« Rappel »). Sinon Safari peut **révoquer l'abonnement**.
- Permission de notifs demandée suite à un geste utilisateur, après installation sur l'écran d'accueil.
- Fournir un petit exemple de client (snippet JS : abonnement + appel API + service worker).

## 9. Limites à assumer et à communiquer

- Apple **ne garantit pas la livraison** des push. Pour des rappels de médicaments, c'est un vrai sujet : à dire clairement aux utilisateurs finaux.
- Si la PWA est retirée de l'écran d'accueil ou la permission révoquée, l'abonnement meurt : le serveur ne peut que le constater à l'échec d'envoi.
- En PWA, aucune notif locale planifiée hors serveur : tout passe par PUSH.

## 10. Vie privée

- Aucune donnée de santé, aucun nom de médicament, aucun texte de rappel sur le serveur.
- L'abonnement push identifie un appareil mais sans info personnelle.
- Le serveur « ne sait pas ce qu'il fait ni pourquoi » : c'est le principe de conception, à préserver dans toute évolution.
- Pas de logs du contenu ; logs minimaux et purgés.

## 11. Déploiement

- Auto-hébergé par Raphaël, **service séparé à côté de PocketBase** (même machine possible, port et cron propres, ne pas mélanger avec PocketBase).
- Déjà en place : reverse proxy + certificat **Let's Encrypt** (HTTPS obligatoire pour le Web Push).
- Cibles de dev de Raphaël : **Linux et Windows** (pas de macOS). Prévoir binaire/Docker simple.
- Contexte infra : Raphaël administre des serveurs Linux et un homelab ; il préfère du léger, sans dépendances lourdes (idéalement un seul process, SQLite, pas de Redis ni broker).

## 12. Pistes d'implémentation

Raphaël n'a pas encore tranché le langage. Pistes évoquées :
- **Partir de `go-notify-server`** (Go, SQLite, VAPID, sans vendor lock-in) et lui ajouter table de planification + cron. **À vérifier : licence** (non confirmée) et absence de planification native.
- Autre projet de référence : `notify` de moureau-dev (API HTTP, CLI, un seul process Bun, SQLite, image Docker).
- Projet d'Akshay-152 : abonnements, campagnes, planification, retries, dashboard admin, Web Push natif sans Firebase.
- Ou **coder le sien** avec une librairie `web-push` (Node, Go, etc.) : estimé à une soirée de travail pour le noyau.
- Ce sont de petits dépôts : **à auditer avant toute confiance**.
- Points techniques : sujet VAPID avec une **vraie adresse de contact** (`mailto:`), Apple rejette les valeurs bidon ; HTTPS obligatoire.

## 13. Questions ouvertes (à trancher au démarrage)

1. Langage / stack : Go, Node/Bun, autre ?
2. Fork de `go-notify-server` ou from scratch ?
3. Format exact de la règle de récurrence (cron-like simplifié, jours de semaine + heure, intervalle ?).
4. Politique de quotas par application et par abonnement (anti-abus).
5. Auth des apps : simple clé d'API, ou signature des requêtes ?
6. Comportement en cas d'arrêt serveur (rattrapage des rappels manqués).
7. Interface d'admin minimale pour enregistrer les applications (CLI suffisante ?).
8. Nom du dépôt : « push » est très générique et difficile à chercher ; envisager un nom de dépôt plus distinctif (`push-relay`, `pushlet`) tout en gardant PUSH comme nom d'usage.
9. Licence du projet (Raphaël envisage de diffuser Phms gratuitement ; PUSH pourrait être open source).

## 14. Premiers pas suggérés pour Claude Code

1. Trancher la stack et initialiser le dépôt (README, licence, `.gitignore`, Dockerfile).
2. Modèle de données SQLite : `apps`, `reminders` (subscription, opaque_id, schedule, tz, manage_token_hash, next_run_at).
3. Génération / chargement des clés VAPID (persistées, avec avertissement de sauvegarde).
4. Les 3 routes + clé publique VAPID + santé, avec auth par clé d'app.
5. Cron minute : sélection, envoi Web Push (payload = identifiant), recalcul des récurrences, nettoyage sur 404/410.
6. Tests : fuseaux et changement d'heure, rappels uniques/récurrents, abonnements morts, isolation entre apps.
7. Client de démo : mini-PWA (service worker + IndexedDB) à tester sur un vrai iPhone ajouté à l'écran d'accueil.
8. Documentation d'intégration pour une PWA tierce, avec le piège iOS « toujours afficher une notif ».
