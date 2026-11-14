# PUSH — notes pour Claude Code

Brief d'origine : [`docs/brief.md`](docs/brief.md). Ce fichier résume les décisions prises et les règles à respecter.

## Principe non négociable

Le serveur **ne sait rien** : il stocke abonnement push + identifiant opaque + planification, et envoie `{"id","t"}`. Jamais de texte de rappel, de nom, de donnée de santé, ni de mention d'une app cliente précise (Phms ou autre) dans le code/l'API. Pas de log d'endpoint ni d'identifiant (compteurs uniquement).

## Décisions (questions ouvertes du brief §13)

| # | Question | Décision |
|---|---|---|
| 1 | Stack | **Go** 1.24, binaire statique unique, `modernc.org/sqlite` (pur Go, pas de CGO → cross-compile Windows/arm64 trivial), `SherClockHolmes/webpush-go` v1.4.0 (MIT). Routeur `net/http` standard. |
| 2 | Fork ou from scratch | **From scratch** : petit noyau, rien à auditer chez un tiers à part webpush-go. |
| 3 | Récurrence | JSON : `once` (`at` RFC 3339), `daily` (`time`, `tz`, `days?`, `start?`, `until?`), `interval` (`every` jours, `start`, `time`, `tz`, `until?`). Évaluée dans le fuseau IANA de l'appareil. |
| 4 | Quotas | Par app : `max_subscriptions` (défaut 10 000) et `max_reminders_per_sub` (défaut 100), réglables via CLI. Rate-limit IP laissé au reverse proxy. |
| 5 | Auth des apps | Clé d'API (`X-App-Key`, stockée hashée) + origines autorisées par app (CORS). Pas de signature : la clé est publique dans une PWA de toute façon. Par appareil : jeton de gestion (`Bearer`, hashé). |
| 6 | Rattrapage après arrêt | Occurrence envoyée si retard ≤ `PUSH_GRACE` (1 h par défaut), sinon sautée. Idem pour les retries. |
| 7 | Admin | CLI (`push app add/list/update/rotate-key/rm`). Pas d'UI web. |
| 8 | Nom du dépôt | `push-unaware-serving-host` (déjà créé). Binaire : `push`. |
| 9 | Licence | MIT (à confirmer par Raphaël). |

Autres choix :
- Endpoints limités aux services push connus (FCM, Mozilla, Apple, WNS) par défaut → pas de SSRF. `PUSH_ALLOW_ANY_PUSH_HOST` pour lever.
- Ré-enregistrer un endpoint existant renouvelle le jeton et garde les rappels (posséder l'abonnement = posséder l'appareil).
- Payload : `{"id","t"}` ; en-têtes `Urgency: high`, `TTL`, `Topic` = hash de l'id (collapse si appareil hors ligne).
- Scan toutes les 15 s (`PUSH_INTERVAL`).
- Contournement webpush-go v1.4.0 : il préfixe `mailto:` à tout sujet non-https → on retire notre `mailto:` avant (sinon `mailto:mailto:`, rejeté par Apple). Testé dans `pushsend_test.go`.

## Arborescence

```
cmd/push/            main : serve + CLI admin
internal/schedule/   parsing et calcul des occurrences (fuseaux, DST)
internal/store/      SQLite : apps, subscriptions, reminders
internal/scheduler/  boucle d'envoi, retries, nettoyage 404/410, grace
internal/pushsend/   envoi Web Push (VAPID, RFC 8291), validation endpoints
internal/api/        HTTP API v1 + CORS
internal/vapid/      génération/chargement de vapid.json (0600, jamais écrasé)
web/demo/            PWA de démo + push-client.js réutilisable (embarqués)
deploy/              systemd, nginx, Caddy
docs/                api.md, integration.md, brief.md
```

## Commandes

```bash
go test ./...          # tous les tests (fuseaux/DST, isolation apps, 410, retries, e2e chiffrement)
make dist              # binaires linux/windows
PUSH_VAPID_SUBJECT=mailto:x@domaine-reel go run ./cmd/push serve -demo
```

## Reste à faire / idées

- Tester la démo sur un vrai iPhone (écran d'accueil) derrière le reverse proxy.
- Sauvegarde : documenter/automatiser la copie de `vapid.json` (+ `push.db` via `sqlite3 .backup`).
- Éventuel `push stats` / métriques Prometheus (compteurs seulement).
- Purge d'abonnements sans rappel depuis longtemps (sans stocker de date d'accès ? à réfléchir).
