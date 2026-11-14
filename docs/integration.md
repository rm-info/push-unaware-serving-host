# Intégrer PUSH dans une PWA

Principe : le serveur ne connaît qu'un **identifiant opaque** et une **planification**. Le texte du rappel reste dans l'IndexedDB de l'appareil ; le service worker le retrouve à la réception du push.

## 0. Prérequis

- PWA servie en **HTTPS**, avec un manifeste (`display: "standalone"`) et un service worker.
- Une app enregistrée sur le serveur : `push app add ma-pwa -origins "https://ma-pwa.example.org"` → clé `app_…`.
- **iOS / iPadOS 16.4+** : le Web Push n'existe que pour une PWA **ajoutée à l'écran d'accueil** et ouverte depuis l'icône. Détectez-le (`display-mode: standalone`) et guidez l'utilisateur (Partager → « Sur l'écran d'accueil »).

## 1. Le client

Copiez [`web/demo/push-client.js`](../web/demo/push-client.js) (module ES, sans dépendance) :

```js
import { PushClient } from './push-client.js';

const registration = await navigator.serviceWorker.register('/sw.js');
const push = new PushClient({
  baseUrl: 'https://push.votre-domaine',
  appKey: 'app_…',
  registration,
  storage, // optionnel ; par défaut localStorage. IndexedDB si le SW doit lire le jeton.
});

// Au démarrage de l'app : s'assure que le serveur connaît l'abonnement actuel.
const { reset } = await push.sync();
if (reset) await push.replaceReminders(allLocalReminders()); // le serveur nous avait oubliés

// Activation : OBLIGATOIREMENT dans un gestionnaire de clic (geste utilisateur).
button.onclick = async () => {
  const { reset } = await push.enable();
  if (reset) await push.replaceReminders(allLocalReminders());
};
```

## 2. Créer / supprimer un rappel

```js
const id = crypto.randomUUID();
await db.put('reminders', { id, text: 'Doliprane 1000', schedule }); // le texte reste local
await push.putReminder(id, schedule);                                 // le serveur ne voit que id + schedule

// Tous les jours à 8h, heure de l'appareil, changement d'heure compris :
const schedule = { type: 'daily', time: '08:00', tz: PushClient.timeZone() };

await push.deleteReminder(id);
```

Toujours écrire en local **avant** d'appeler le serveur : si le push arrive, le texte doit exister.

Resynchronisation complète (ex. au démarrage, ou après une restauration de données) :

```js
await push.replaceReminders(localList.map(r => ({ id: r.id, schedule: r.schedule })));
```

## 3. Le service worker

```js
self.addEventListener('push', (event) => {
  event.waitUntil((async () => {
    let data = {};
    try { data = event.data.json(); } catch (_) {}
    const reminder = data.id ? await getFromIndexedDB('reminders', data.id) : null;
    // RÈGLE iOS : TOUJOURS afficher une notification, même si l'id est inconnu.
    await self.registration.showNotification(reminder?.text || 'Rappel', {
      tag: data.id,
      data: { id: data.id },
    });
  })().catch(() => self.registration.showNotification('Rappel')));
});
```

**Ne jamais** recevoir un push sans afficher de notification : Safari peut révoquer l'abonnement après quelques push « silencieux ». Le `.catch` final garantit l'affichage même en cas d'erreur IndexedDB.

`data.t` contient l'heure prévue (secondes Unix) si vous voulez afficher « prévu à 8h00 » quand la notif arrive en retard.

Optionnel (Chrome/Firefox ; Safari ne l'émet pas) : gérer `pushsubscriptionchange` en appelant `PUT /v1/subscription` avec le nouvel abonnement — voir [`web/demo/sw.js`](../web/demo/sw.js). Sur Safari, `push.sync()` au démarrage couvre le cas.

## 4. Cycle de vie et pièges

| Situation | Effet | Que faire |
|---|---|---|
| PWA retirée de l'écran d'accueil / permission révoquée | L'abonnement meurt ; le serveur le supprime au prochain envoi (410) | Rien côté serveur. Au prochain lancement, `sync()` renvoie `reset: true` → `replaceReminders()` |
| Données locales effacées | Jeton perdu | `sync()` réenregistre l'appareil (nouveau jeton), `reset: true` |
| Serveur PUSH arrêté | Occurrences > `PUSH_GRACE` sautées | Prévenir l'utilisateur que la livraison n'est pas garantie |
| Plusieurs prises par jour | — | Un rappel par horaire |
| Rappel unique passé | Refusé (`422 no_occurrence`) par `PUT /v1/reminders/{id}`, ignoré par `PUT /v1/reminders` | — |

## 5. Ce qu'il faut dire aux utilisateurs

- Les notifications passent par les serveurs d'Apple/Google/Mozilla, qui **ne garantissent pas** la livraison ni l'heure exacte.
- Aucun contenu n'est envoyé au serveur : seulement un identifiant aléatoire et une heure.
- Sur iPhone, l'app doit rester sur l'écran d'accueil et les notifications autorisées.
