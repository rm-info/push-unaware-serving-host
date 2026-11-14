# API PUSH v1

Toutes les réponses sont en JSON. Erreurs : `{"error": "<code>", "message": "<texte>"}`.

## Authentification

- `X-App-Key: app_…` — clé de l'application (créée par `push app add`). Requise sur toutes les routes `/v1/` sauf la clé VAPID.
- `Authorization: Bearer sub_…` — **jeton de gestion** de l'appareil, renvoyé par `POST /v1/subscriptions`. Requis pour toutes les routes de rappels et `/v1/subscription`.

Un jeton n'est valable qu'avec la clé de l'app qui l'a émis : les apps sont isolées.

CORS : si l'app a des origines configurées, une requête navigateur venant d'une autre origine reçoit `403 origin_not_allowed`. Sans origines configurées, toute origine est acceptée.

## Routes publiques

### `GET /healthz`
`200 {"status":"ok"}` ou `503 {"status":"unavailable"}`.

### `GET /v1/vapid-public-key`
`200 {"publicKey":"B…"}` — à passer en `applicationServerKey` à `pushManager.subscribe()`.

## Appareil (abonnement)

### `POST /v1/subscriptions`
Corps : le résultat de `PushSubscription.toJSON()`.

```json
{"endpoint":"https://web.push.apple.com/…","expirationTime":null,"keys":{"p256dh":"B…","auth":"…"}}
```

- `201 {"token":"sub_…","created":true}` — nouvel appareil.
- `200 {"token":"sub_…","created":false}` — endpoint déjà connu pour cette app : clés mises à jour, **jeton renouvelé** (l'ancien ne marche plus), rappels conservés. Posséder l'abonnement prouve la possession de l'appareil.
- `422 invalid_endpoint` (pas https, ou service push inconnu), `422 invalid_keys`, `429 quota_exceeded`.

Le jeton n'est stocké que sous forme de hash : conservez-le côté client.

### `PUT /v1/subscription`
Remplace endpoint et clés (ex. après `pushsubscriptionchange`) en gardant jeton et rappels. Même corps que ci-dessus. `204`, ou `409 endpoint_exists` si cet endpoint est déjà enregistré sous un autre jeton (réenregistrez-vous alors avec `POST /v1/subscriptions`).

### `DELETE /v1/subscription`
Oublie l'appareil et tous ses rappels. `204`.

## Rappels

`{id}` : identifiant opaque choisi par le client, 1 à 128 caractères `[A-Za-z0-9_-]` (un UUID convient). Unique par appareil.

### `PUT /v1/reminders/{id}`
```json
{"schedule": {"type":"daily","time":"08:00","tz":"Europe/Paris"}}
```
- `201` (créé) ou `200` (remplacé) : `{"id":"…","schedule":{…normalisé…},"next_run_at":"2026-11-15T07:00:00Z"}`
- `422 invalid_id | invalid_schedule | no_occurrence` (ex. `once` dans le passé), `429 quota_exceeded`.

### `DELETE /v1/reminders/{id}`
`204`, ou `404 not_found`.

### `GET /v1/reminders`
`200 {"reminders":[{"id":"…","schedule":{…},"next_run_at":"…"}]}` — triés par prochaine échéance.

### `PUT /v1/reminders`
Remplace **atomiquement** toute la liste de l'appareil (resynchronisation) :
```json
{"reminders":[{"id":"a","schedule":{…}},{"id":"b","schedule":{…}}]}
```
Les rappels sans occurrence future (ex. `once` passé) sont ignorés silencieusement. Réponse : la liste résultante (comme `GET`). Erreurs : `422` avec `index` de l'élément fautif, `422 duplicate_id`, `429 quota_exceeded`.

## Planifications

| Type | Champs | Exemple |
|---|---|---|
| `once` | `at` : RFC 3339 avec décalage | `{"type":"once","at":"2026-11-15T08:00:00+01:00"}` |
| `daily` | `time` `HH:MM`, `tz` IANA ; optionnels : `days` (`mon`…`sun`), `start`, `until` (`YYYY-MM-DD`, inclus, en date locale) | `{"type":"daily","time":"08:00","tz":"Europe/Paris","days":["mon","fri"]}` |
| `interval` | `every` (1–366 jours), `start`, `time`, `tz` ; optionnel : `until` | `{"type":"interval","every":2,"start":"2026-11-15","time":"21:00","tz":"Europe/Paris"}` |

Fuseau : toujours celui de l'appareil (`Intl.DateTimeFormat().resolvedOptions().timeZone`). L'heure murale est conservée lors des changements d'heure. Une heure inexistante (ex. 02:30 le jour du passage à l'heure d'été) est décalée à une heure valide **du même jour** ; une heure ambiguë (passage à l'heure d'hiver) ne déclenche qu'une fois.

Plusieurs prises par jour = plusieurs rappels (un par heure).

## Ce que reçoit le service worker

Payload (chiffré de bout en bout, RFC 8291) :

```json
{"id":"<identifiant opaque>","t":1763190000}
```

`t` = heure prévue (secondes Unix UTC), utile si la notification arrive en retard. Le push est envoyé avec `Urgency: high`, `TTL` = `PUSH_TTL`, et un en-tête `Topic` dérivé de l'identifiant : si l'appareil était hors ligne, seul le dernier push en attente d'un même rappel est livré.

## Envoi : comportement serveur

- Scan toutes les `PUSH_INTERVAL` (15 s par défaut).
- `2xx` : `once` supprimé, récurrent avancé à l'occurrence suivante.
- `404`/`410` : abonnement mort → supprimé avec **tous** ses rappels.
- `429`/`5xx`/erreur réseau : retry (30 s, 1 min, 2 min… plafonné à 10 min, `Retry-After` respecté) tant que le retard reste ≤ `PUSH_GRACE`, sinon occurrence abandonnée.
- Autres `4xx` : occurrence abandonnée (retenter ne changerait rien).
- Serveur arrêté : au redémarrage, les occurrences en retard de plus de `PUSH_GRACE` sont **sautées** (pas de rafale de vieux rappels), les autres envoyées.
