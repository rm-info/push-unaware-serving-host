# PUSH — Push Unaware Serving Host

Petit serveur **auto-hébergé, générique et qui ne sait rien** : une PWA lui dit « envoie un Web Push à cet appareil à telle heure, avec cet identifiant opaque », et il s'exécute au bon moment, sans connaître ni le contenu, ni le but, ni l'utilisateur.

- **Un seul binaire Go**, SQLite embarqué (pur Go, sans CGO) : Linux, Windows, arm64, Docker.
- **Multi-applications** : chaque PWA a sa clé d'API, ses quotas, ses origines autorisées ; données isolées.
- **Zéro contenu côté serveur** : le push ne transporte que `{"id": "<uuid>", "t": <heure prévue>}`. Le service worker retrouve le texte en local (IndexedDB).
- **Récurrences avec fuseau IANA** : « tous les jours à 8h, Europe/Paris » reste à 8h après le changement d'heure.
- Nettoyage automatique des abonnements morts (404/410), retries avec backoff, rattrapage borné après une coupure.

> ⚠️ Apple (et les autres services push) **ne garantissent pas la livraison**. Pour des rappels critiques (médicaments), dites-le clairement aux utilisateurs finaux.

## Démarrage rapide

```bash
# Binaire
make build                      # ou : go build -o push ./cmd/push
./push app add ma-pwa -origins "https://ma-pwa.example.org"   # affiche la clé d'API (une seule fois)
PUSH_VAPID_SUBJECT=mailto:vous@votre-domaine ./push serve     # écoute sur 127.0.0.1:8080
```

Au premier `serve`, la paire de clés VAPID est générée dans `data/vapid.json`.
**Sauvegardez ce fichier.** Un abonnement push est lié à la clé publique VAPID qui l'a créé : si elle est perdue ou changée, **tous les abonnements de toutes les apps meurent**.

Docker :

```bash
mkdir data && sudo chown 65532:65532 data   # l'image tourne en uid 65532
# éditez PUSH_VAPID_SUBJECT dans docker-compose.yml, puis :
docker compose up -d
docker compose exec push push app add ma-pwa -origins "https://ma-pwa.example.org"
```

Le Web Push exige HTTPS : placez PUSH derrière votre reverse proxy (exemples dans [`deploy/`](deploy/) : nginx, Caddy, unité systemd durcie).

## Configuration

Variables d'environnement (ou flags équivalents, `push serve -h`) :

| Variable | Défaut | Rôle |
|---|---|---|
| `PUSH_VAPID_SUBJECT` | — (**requis**) | Contact VAPID, `mailto:…` ou `https://…`. Doit être réel : Apple rejette les valeurs bidon. |
| `PUSH_DATA_DIR` | `./data` | Base SQLite `push.db` + `vapid.json`. |
| `PUSH_LISTEN` | `127.0.0.1:8080` | Adresse d'écoute (`0.0.0.0:8080` dans l'image Docker). |
| `PUSH_GRACE` | `1h` | Retard max pour envoyer une occurrence (coupure serveur, retries). Au-delà : occurrence **sautée**, pas envoyée en retard. |
| `PUSH_TTL` | `1h` | Durée pendant laquelle le service push garde un message si l'appareil est hors ligne. |
| `PUSH_INTERVAL` | `15s` | Fréquence de scan des rappels dus. |
| `PUSH_ALLOW_ANY_PUSH_HOST` | `false` | Par défaut seuls les services push connus (Google, Mozilla, Apple, Microsoft) sont acceptés comme endpoint : évite que le serveur serve de relais HTTP (SSRF). |
| `PUSH_DEMO` | `false` | Sert la PWA de démo sous `/demo/`. |
| `PUSH_DEBUG` | `false` | Logs détaillés. |

## Administration (CLI)

```text
push app add <nom> [-origins "https://a https://b"] [-max-subscriptions 10000] [-max-reminders 100]
push app list
push app update <nom> [-origins …] [-max-subscriptions N] [-max-reminders N]
push app rotate-key <nom>
push app rm <nom> [-yes]
push vapid public-key
```

La CLI peut tourner pendant que le serveur tourne (SQLite en WAL).

## API

Résumé (détails : [`docs/api.md`](docs/api.md)) :

| Route | Auth | Rôle |
|---|---|---|
| `GET /healthz` | — | Santé |
| `GET /v1/vapid-public-key` | — | Clé publique VAPID |
| `POST /v1/subscriptions` | clé d'app | Enregistre l'appareil, renvoie le **jeton de gestion** |
| `PUT /v1/subscription` | clé + jeton | Met à jour l'abonnement (rotation côté navigateur) |
| `DELETE /v1/subscription` | clé + jeton | Oublie l'appareil et ses rappels |
| `PUT /v1/reminders/{id}` | clé + jeton | Crée/remplace un rappel |
| `DELETE /v1/reminders/{id}` | clé + jeton | Supprime un rappel |
| `GET /v1/reminders` | clé + jeton | Liste (resynchro) |
| `PUT /v1/reminders` | clé + jeton | Remplace toute la liste (resynchro) |

Planifications :

```json
{"type":"once","at":"2026-11-15T08:00:00+01:00"}
{"type":"daily","time":"08:00","tz":"Europe/Paris"}
{"type":"daily","time":"20:00","tz":"Europe/Paris","days":["mon","wed","fri"],"until":"2026-12-31"}
{"type":"interval","every":2,"start":"2026-11-15","time":"21:00","tz":"Europe/Paris"}
```

## Intégrer une PWA

Voir [`docs/integration.md`](docs/integration.md) et le client réutilisable [`web/demo/push-client.js`](web/demo/push-client.js).
Piège n°1 sur iOS : **chaque push doit afficher une notification visible**, même si l'identifiant est inconnu localement — sinon Safari peut révoquer l'abonnement.

Pour tester sur un vrai iPhone : `PUSH_DEMO=true`, créez une app avec l'origine de votre serveur, ouvrez `https://push.votre-domaine/demo/`, ajoutez-la à l'écran d'accueil, ouvrez-la depuis l'icône, collez la clé d'API, activez.

## Vie privée — ce que le serveur stocke

| Donnée | Pourquoi |
|---|---|
| Apps : nom, **hash** de la clé, origines, quotas | Authentification, isolation, anti-abus |
| Abonnements : endpoint + clés `p256dh`/`auth`, **hash** du jeton | Envoyer le push chiffré |
| Rappels : identifiant opaque, planification, prochaine échéance | Savoir quand envoyer quoi |

Rien d'autre : pas de texte, pas de compte, pas d'IP, pas d'horodatage d'accès. Les logs ne contiennent que des compteurs (envoyés, sautés, échecs…) et jamais d'endpoint ni d'identifiant. Désactivez aussi les access logs de votre reverse proxy (exemples fournis).

## Limites connues

- Livraison non garantie par Apple/Google/Mozilla.
- Si la PWA est retirée de l'écran d'accueil ou la permission révoquée, le serveur ne le découvre qu'au prochain envoi (404/410) et nettoie alors.
- La clé d'API d'une PWA est forcément visible dans son code : c'est un **identifiant d'app** plus qu'un secret. La protection réelle vient des origines autorisées (navigateurs), des quotas, de la restriction aux services push connus, et du rate-limit côté reverse proxy.

## Développement

```bash
make test    # go vet + go test ./...
make dist    # binaires linux amd64/arm64 + windows amd64 dans dist/
```

Licence : MIT.
