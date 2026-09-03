# Frame Social API

> Frame Social mobil uygulaması için **tek deploy, modüler, güvenli** Go backend.
> Telefon OTP ile kimlik doğrulama, profil/gizlilik, arkadaşlık, engelleme, bire-bir ve **grup sohbetleri**, RAM-first mesajlaşma, tepki/yanıt/kaydetme, okundu bilgisi, **üç modlu mesaj silme**, SSE/Long-poll olayları ve FCM push’u tek bir serviste sunar. Veriler harici **SurrealDB**’de tutulur, istemci **Flutter**’dır.

**Durum:** Production-ready · Railway + Docker · SurrealDB RPC · Firebase Opsiyonel
**Flutter için tam endpoint kataloğu:** [`FLUTTER_API_REHBERI.md`](./FLUTTER_API_REHBERI.md)

---

## İçindekiler

- [Özellikler](#özellikler)
- [Teknoloji Yığını](#teknoloji-yığını)
- [Mimari ve Proje Yapısı](#mimari-ve-proje-yapısı)
- [Güvenlik Modeli](#güvenlik-modeli)
- [Ortam Değişkenleri](#ortam-değişkenleri)
- [Hızlı Başlangıç](#hızlı-başlangıç)
- [API Genel Bakış](#api-genel-bakış)
- [Ayrıntılı Modüller](#ayrıntılı-modüller)
- [Push Bildirimleri (FCM)](#push-bildirimleri-fcm)
- [Flutter Entegrasyon Notları](#flutter-entegrasyon-notları)
- [Test, Kalite ve CI](#test-kalite-ve-ci)
- [Dağıtım Checklist](#dağıtım-checklist)

---

## Özellikler

| Alan | Kapsam |
|---|---|
| **Kimlik Doğrulama** | OTP `signup`/`login` (HMAC-SHA256), `verify`, `refresh` (SHA-256), `logout`, JWT HS256 (`15dk`), rate-limit OTP/verify |
| **Hesap** | `GET/PATCH /v1/me`, `DELETE /v1/me` (anonimleştirme + session/push/friendship temizliği) |
| **Gizlilik** | `GET/PATCH /v1/me/privacy` (`isPrivate`, `messagePermission`, `friendRequestPermission` vs.) |
| **Sosyal** | Kullanıcı arama, arkadaşlık isteği gönder/kabul/ret, `unfriend`, engelle/engel kaldır, engellenenler listesi |
| **Sohbet** | Bire-bir `direct` (pair_key), grup, `GET /v1/conversations` (kind-aware), `GET/POST /conversations/{id}/messages` (pagination, RAM-merge) |
| **Mesaj** | `cleanMessageBody`, dedup `clientId`, `50/60sn` limit, `replyToId`, `read/delivered` receipt, `reactions` (1/ user), `saved`, `receipt` |
| **Mesaj Silme** | `DELETE for-me` (per-user `message_hidden`), `DELETE for-everyone` (global scrub + tombstone), `POST retract` (güvenli `kind:deleted` placeholder — Flutter lokalize eder) |
| **Gruplar** | Unique `group_id`, `owner/admin/member`, `name/description/image/access` ayrı endpointler, `public/private/password` + bcrypt, arama gizliliği, davet/katılım isteği (idempotent), üye listesi/çıkarma/ayrılma/ownership transfer/rol |
| **Olaylar** | `GET /v1/events/messages?after=` (25sn long-poll) + `GET /v1/events/stream` (SSE), `messageEventBroker` (per-account version) |
| **Push** | FCM multicast (500/batch), token dedup, geçersiz token temizliği, kuyruk `1000`, sender hariç |

---

## Teknoloji Yığını

- **Dil:** Go `1.27` (`go.mod`’de pinli)
- **HTTP:** `net/http` ServeMux (Go 1.22 pattern `METHOD /path`), `slog` JSON log, `X-Request-ID`, `Recoverer`, `SecurityHeaders`
- **DB:** SurrealDB HTTP RPC (`internal/database/surreal.go`, BasicAuth + `NS/DB` + `SURREAL_PROXY_TOKEN`)
- **Auth:** `golang-jwt/jwt/v5`, `nyaruka/phonenumbers`, `golang.org/x/crypto/bcrypt` (grup şifresi)
- **Push:** `firebase.google.com/go/v4` (opsiyonel)
- **Deploy:** Multi-stage `golang:1.27-alpine` → `alpine:3.22` (non-root `app`, `ca-certificates`, `tzdata`)

---

## Mimari ve Proje Yapısı

**Prensip:** Tek deploy, **modüler monolith**. Her özellik kendi handler/store/routes üçlüsünde; ortak kesitler merkezî.

```
cmd/api/main.go          → config → SurrealDB → api.New → http.Server (graceful shutdown)
internal/
  api/
    server.go            → Server struct + New/handler/health
    router.go            → routes() — tek uyum sınırı, feature register delegasyonu
    middleware.go        → requireAuth, requestID, clientIP, recoverer, securityHeaders
    http_rate_limit.go   → rateLimiter / endpointLimiter (account:bucket)
    auth.go + auth_routes.go
    accounts.go + accounts_routes.go
    users.go + users_routes.go
    friends.go + friends_routes.go
    blocking.go + blocking_routes.go
    privacy.go + privacy_routes.go
    conversations.go + conversations_routes.go   (list/createDirect, kind-aware)
    groups.go + group_store.go + group_password.go + group_requests.go + group_members.go + groups_routes.go
    messages.go + messages_routes.go             (list/send/reaction/saved/receipt)
    message_deletion.go + message_deletion_store.go  (for-me / for-everyone / retract)
    persist.go + cache.go (memberCache, pendingStore) + events.go (broker) + push_sender.go + push_devices.go
  config/ + security/ + database/
```

**İstek yaşam döngüsü (mesaj):** `requireAuth` → `messageRateLimiter.Check` (memory, `50/60sn` + dedup `24s`) → `lockMessageAdmission` (64 shard) → `persist.accept` (bounded `pendingStore` + `chan 10000`, monotonic `createdAt`) → `events.publish` → `triggerPushForMessage` (queue 1000) → `200/201`.
Arka plan `persister.loop` → `doPersist` (tombstone kontrolü, transaction, `message_receipt`, `last_message`) → `pending.Remove`. Silme `withPersistenceLock` ile aynı `mu`’yu paylaşır, pending’i anında gizler/retract eder, `publishConversation`.

### Tasarım Kararları

- **RAM-first:** Kabul edilen mesaj anında iki cihaza görünür, DB yazımı sıralı retry’lı (`200ms → 10s` backoff).
- **Idempotency:** `clientId` dedup (memory + Surreal `message_dedup`), grup davet/join `pending` varsa aynı `id` döner, `DELETE`/`POST` tekrarları `204`.
- **Parola:** Grup şifresi `bcrypt.DefaultCost`, plaintext asla saklanmaz/loglanmaz.

---

## Güvenlik Modeli

- Bu repo **hiçbir üretim sırrı içermez**. Tüm hassas değerler runtime’da env’den.
- `.env`, `*.pem`, `service-account.json` hem `.gitignore` hem `.dockerignore`’da.
- `PUBLIC_BASE_URL` prod’da HTTPS zorunlu; SurrealDB portunu doğrudan açma, reverse proxy arkasında tut.
- `APP_ENV=production` iken `OTP_MODE=development` ile boot engellenir (secret drift guard + JWT round-trip check `refresh`’te).
- Tüm Surreal sorgular **bound parametreli**, `validRecord` (` ;'"` reddi), `decode` 1MiB + `DisallowUnknownFields`, `phone` `phonenumbers` ile E.164.

> **Kural:** Sırrı GitHub’a veya imaja koyma. Railway/Sunucu secret yönetimini kullan.

---

## Ortam Değişkenleri

`.env.example` sadece placeholder. Prod’da güçlü rastgele üret.

| Değişken | Zorunlu | Açıklama |
|---|---|---|
| `APP_ENV` |  | `development` / `production` |
| `HTTP_ADDR` |  | Dinlenecek adres (`:3000`). `PORT` varsa override etmez |
| `PORT` | Railway | Platformun verdiği port — elle ekleme |
| `PUBLIC_BASE_URL` | Evet | Dış HTTPS adresi |
| `SURREAL_URL` | Evet | SurrealDB HTTPS |
| `SURREAL_NAMESPACE` | Evet |  |
| `SURREAL_DATABASE` | Evet |  |
| `SURREAL_USERNAME` | Evet | Sınırlı backend user |
| `SURREAL_PASSWORD` | Evet |  |
| `SURREAL_PROXY_TOKEN` | Evet | Güçlü 64+ char |
| `JWT_SECRET` | Evet | ≥32 char, `OTP_PEPPER`’dan farklı |
| `OTP_PEPPER` | Evet | ≥32 char |
| `OTP_MODE` | Evet | `development` (kod `debugCode` döner) / prod SMS modu |
| `ACCESS_TOKEN_MINUTES` |  | Varsayılan `15` |
| `REFRESH_TOKEN_DAYS` |  | Varsayılan `30` |
| `FIREBASE_PROJECT_ID` |  | Opsiyonel (JSON içinde varsa gerekmez) |
| `FIREBASE_SERVICE_ACCOUNT_JSON` |  | Tek satır JSON — **gizli** |
| `FIREBASE_SERVICE_ACCOUNT_BASE64` |  | Base64 alternatifi |
| `FIREBASE_CREDENTIALS_JSON` / `GOOGLE_APPLICATION_CREDENTIALS_JSON` / `FIREBASE_CREDENTIALS_BASE64` |  | Alternatif alias’lar |

---

## Hızlı Başlangıç

### Yerel

```bash
cp .env.example .env
# .env içindeki HER placeholder'ı yerel güçlü değerlerle değiştir
set -a; . ./.env; set +a
go run ./cmd/api
curl http://127.0.0.1:3000/health
# {"status":"ok"}
```

### Docker

```bash
docker build -t frame-social-api .
docker run --rm -p 3000:3000 --env-file /secure/path/frame-social.env frame-social-api
# Railway PORT'u otomatik sağlar: 0.0.0.0:$PORT
```

### Railway

- `PORT`’u manuel ekleme.
- Tüm env’leri **Variables**’da tanımla, `SURREAL_PROXY_TOKEN` + `JWT_SECRET`’ı 64 char üret.
- Healthcheck: `GET /health`.

---

## API Genel Bakış

Korunan uçlar `Authorization: Bearer <accessToken>` ister. Detaylı istek/yanıt örnekleri için **[`FLUTTER_API_REHBERI.md`](./FLUTTER_API_REHBERI.md)**’ye bak.

| Grup | Uçlar |
|---|---|
| **Health** | `GET /health` |
| **Auth** | `POST /v1/auth/signup/request`, `POST /v1/auth/signup/verify`, `POST /v1/auth/login/request`, `POST /v1/auth/login/verify`, `POST /v1/auth/refresh`, `POST /v1/auth/logout` |
| **Account** | `GET /v1/me`, `PATCH /v1/me`, `DELETE /v1/me` |
| **Privacy** | `GET /v1/me/privacy`, `PATCH /v1/me/privacy` |
| **Users/Friends** | `GET /v1/users/search?q=`, `POST /v1/friends/requests`, `GET /v1/friends/requests`, `POST /v1/friends/requests/{id}/respond`, `DELETE /v1/friends/{id}` |
| **Blocking** | `POST /v1/users/{id}/block`, `DELETE /v1/users/{id}/block`, `GET /v1/me/blocked-users` |
| **Conversations** | `GET /v1/conversations`, `POST /v1/conversations/direct` |
| **Groups** | `POST /v1/groups`, `GET /v1/groups/search?q=`, `PATCH /v1/groups/{id}/name\|description\|image\|access`, `POST /v1/groups/{id}/join`, `POST /v1/groups/{id}/invitations`, `POST /v1/groups/{id}/invitations/{invitationId}/accept|reject`, `DELETE /v1/groups/{id}/invitations/{invitationId}`, `POST /v1/groups/{id}/join-requests`, `POST /v1/groups/{id}/join-requests/{requestId}/approve|reject`, `DELETE /v1/groups/{id}/join-requests/{requestId}`, `GET /v1/groups/{id}/members`, `POST /v1/groups/{id}/leave`, `DELETE /v1/groups/{id}/members/{userId}`, `POST /v1/groups/{id}/ownership`, `PATCH /v1/groups/{id}/members/{userId}/role` |
| **Messages** | `GET /v1/conversations/{id}/messages`, `POST /v1/conversations/{id}/messages`, `POST /v1/conversations/{id}/read`, `POST /v1/conversations/{id}/delivered`, `PUT /v1/messages/{id}/reactions`, `DELETE /v1/messages/{id}/reactions/{emoji}`, `PUT /v1/messages/{id}/saved`, `DELETE /v1/messages/{id}/saved`, `POST /v1/messages/{id}/receipt` |
| **Deletion** | `DELETE /v1/messages/{id}/for-me`, `DELETE /v1/messages/{id}/for-everyone`, `POST /v1/messages/{id}/retract` |
| **Push** | `PUT /v1/me/push-devices`, `DELETE /v1/me/push-devices/{deviceId}` |
| **Events** | `GET /v1/events/messages?after=`, `GET /v1/events/stream?after=` (SSE) |

Ortak hata: `{ "error": { "code": "...", "message": "..." } }` + `Retry-After` (429). Kod listesi için Flutter rehberine bak.

---

## Ayrıntılı Modüller

### Gruplar — Ayrı Dosyalarda, Genişletilebilir

- **Oluşturma:** `id` `^[a-z0-9][a-z0-9_-]{2,48}$`, unique (`409 group_exists`), creator `owner`. `group_id` ayrı indexed alan, aranabilir.
- **Bilgi:** `name` (2..80), `description` (0..500), `imageUrl` (≤2000) ayrı `PATCH`’ler, sadece `owner|admin`.
- **Erişim:** `privacy=public|private`, `joinRule=open|invite|approval|password` (`PATCH /access` sadece `owner|admin`, `password` bcrypt).
- **Arama:** `GET /groups/search?q=` — `private` gruplar sadece üyelere döner, hash asla dönmez.
- **Davet/İstek:** Ayrı `group_invitation` / `group_join_request` tabloları, `pending` varsa aynı `id` idempotent. Block’lı çift davet edemez (`PairKey`).
- **Üyelik:** `owner/admin/member`, `GET members`, `POST leave` (tek owner `409`), `DELETE members/{userId}` (owner silinmez, admin→admin yasak), `POST ownership`, `PATCH role` (sadece owner).

### Mesaj Silme — İzole Modül

- `for-me` → `message_hidden` edge + `pending.Hide` (diğer katılımcı görmeye devam).
- `for-everyone` → sadece sender, `message_tombstone: hash(message)` + `body=NONE`, `kind=deleted`, list’ten tamamen gizlenir.
- `retract` → sadece sender, `body=null`, `kind=deleted`, `deleted=true`, `deletedAt` set, liste marker döner — Flutter `“Bu mesaj silindi.”` lokalize eder. Backend display text üretmez.
- Tümü idempotent, `pending` + `persist` (`withPersistenceLock`) + `publishConversation` ile cache/pagination/retry’dan geri gelmez, içerik loglanmaz.

### Mesaj Sistemi

- `GET` pending merge (`IsHidden`, `clientId` dedup, `before` cursor), `readReceiptsEnabled=false` ise `read→delivered`.
- `POST` double-checked dedup + `message_hidden` filtre dahil.

---

## Push Bildirimleri (FCM)

`POST /v1/conversations/{id}/messages` sonrası RAM publish + Surreal sıralı yazım. Alıcıların `push_device` tokenları toplanır, dedup edilir, sender hariç. FCM `SendEachForMulticast` (500/batch, `high` priority + `apns-priority:10`). Geçersiz `not-registered` tokenlar `DeleteByTokens` ile silinir; push hatası mesajı fail etmez.

**Payload:**
```json
{
  "notification": { "title": "<displayName>", "body": "Yeni bir mesajın var" },
  "data": { "type": "new_message", "conversationId": "conversation:xxx", "messageId": "message:xxx", "senderId": "account:xxx" }
}
```

**Uçlar:**

- `PUT /v1/me/push-devices` `{token 10..4096, platform ios|android|web, deviceId 8..200}` → `200` upsert `(account,deviceId)` unique.
- `DELETE /v1/me/push-devices/{deviceId}` → `204` idempotent.

Env: `FIREBASE_SERVICE_ACCOUNT_JSON` veya `BASE64` set → `firebasePusher`, yoksa `noopPusher` (mesaj etkilenmez).

---

## Flutter Entegrasyon Notları

- `FLUTTER_API_REHBERI.md` tek kaynak — tüm istek/yanıt örnekleri orada.
- `401 invalid_token` → `refresh`, `401 invalid_refresh_token` → login. `X-Request-ID`’yi logla, `429`’ta `Retry-After`’a uy.
- Silinen mesaj: `kind=="deleted"` veya `deleted==true` + `body==null` → yerelleştirilmiş placeholder göster.
- Gruplar: `conversation.kind=="group"` ise `name/description/imageUrl` göster, `otherMember`’ı yoksay. Arama `private` gruplar için üye değilse boş döner.
- Push: Login/refresh sonrası `PUT /push-devices` çağır, logout’ta `DELETE`. Data mesajında `conversationId` ile ilgili sohbete yönlendir.
- Events: `GET /events/stream?after=<version>` SSE ana yol, koparsa `GET /events/messages?after=` long-poll + `resync==true` ise `GET /conversations` + `GET /messages` full re-fetch.

---

## Test, Kalite ve CI

```bash
gofmt -w .
go vet ./...
go test ./...
go test -race ./internal/api
```

- `internal/api`’de `httptest` + in-memory `queryer` mock’lar (SurrealDB/Firebase gerektirmez).
- Kapsam: `social_account_test`, `reaction`, `push`, `message_deletion`, `groups` (route/auth/privacy/idempotency/concurrency), `bench`, `integration_determinism`, `resilience`, `rate_limit`.
- `git diff --check` whitespace guard.
- GitHub Actions (`.github/workflows/ci.yml`) her push/PR’da `gofmt` + `vet` + `test` + `race`.

---

## Dağıtım Checklist

- [ ] Tüm env’ler güçlü ve farklı (`JWT_SECRET` ≠ `OTP_PEPPER`, `SURREAL_PROXY_TOKEN` 64+).
- [ ] `APP_ENV=production` → `OTP_MODE` prod SMS moduna alınmış.
- [ ] `PUBLIC_BASE_URL` HTTPS, reverse proxy arkasında, SurrealDB portu kapalı.
- [ ] `FIREBASE_SERVICE_ACCOUNT_*` sadece secret store’da.
- [ ] `docker build` + `go test -race` + `gofmt` + `git diff --check` yeşil.
- [ ] Railway `PORT` manuel eklenmedi.

---

**Lisans:** Özel — Frame Social. Sorun/öneri için issue açın veya `FLUTTER_API_REHBERI.md` üzerinden entegrasyon talebi bırakın.
