# Frame Social API

Frame Social mobil uygulamasının bağımsız Go backend servisidir. Telefon OTP kimlik doğrulama, profil ve gizlilik, arkadaşlık, sohbet, mesaj tepkileri, yanıtlar ve okundu bilgisi uçlarını sunar. Veriler harici bir SurrealDB sunucusunda tutulur.

## Güvenlik modeli

Bu depo hiçbir üretim anahtarı, parola veya veritabanı adresi içermez. Tüm hassas değerler çalışma zamanında ortam değişkenlerinden alınır. `.env`, sertifika, özel anahtar ve kimlik bilgisi dosyaları Git ve Docker bağlamından hariç tutulur.

Üretim sırlarını GitHub'a veya Docker imajına koymayın. Sunucunuzun secret/environment yönetimini kullanın.

## Gerekli ortam değişkenleri

`.env.example` yalnızca yer tutucu değerler içerir. Yeni ve güçlü değerleri sunucuda oluşturun.

| Değişken | Açıklama |
| --- | --- |
| `APP_ENV` | `development` veya `production` |
| `HTTP_ADDR` | Dinlenecek adres; container için `:3000` |
| `PORT` | Render tarafından sağlanan port; `HTTP_ADDR` yoksa otomatik kullanılır |
| `PUBLIC_BASE_URL` | API'nin dışarıdan erişilen HTTPS adresi |
| `SURREAL_URL` | Harici SurrealDB HTTPS adresi |
| `SURREAL_NAMESPACE` | SurrealDB namespace |
| `SURREAL_DATABASE` | SurrealDB database |
| `SURREAL_USERNAME` | Sınırlı yetkili backend kullanıcısı |
| `SURREAL_PASSWORD` | Backend kullanıcısının parolası |
| `SURREAL_PROXY_TOKEN` | SurrealDB HTTPS geçidinin güçlü erişim anahtarı |
| `JWT_SECRET` | En az 32 karakterlik rastgele imzalama anahtarı |
| `OTP_PEPPER` | En az 32 karakterlik, JWT anahtarından farklı rastgele değer |
| `OTP_MODE` | Geliştirmede `development`; üretimde gerçek SMS sağlayıcı modu |
| `ACCESS_TOKEN_MINUTES` | Access token ömrü; varsayılan `15` |
| `REFRESH_TOKEN_DAYS` | Refresh token ömrü; varsayılan `30` |
| `FIREBASE_PROJECT_ID` | Firebase proje kimliği (opsiyonel, JSON içinde varsa gerekmez) |
| `FIREBASE_SERVICE_ACCOUNT_JSON` | Firebase servis hesabı JSON içeriği (tek satır). Gizli tutun, Git'e eklemeyin |
| `FIREBASE_SERVICE_ACCOUNT_BASE64` | Alternatif: JSON'un base64 kodlanmış hali (satır sonu sorunları için) |

Firebase push bildirimleri yalnızca `FIREBASE_SERVICE_ACCOUNT_JSON` veya `FIREBASE_SERVICE_ACCOUNT_BASE64` tanımlıysa aktiftir. Tanımlı değilse servis bildirim göndermeden çalışmaya devam eder ve push hataları mesaj gönderimini etkilemez. Alternatif anahtarlar `FIREBASE_CREDENTIALS_JSON`, `GOOGLE_APPLICATION_CREDENTIALS_JSON` ve `FIREBASE_CREDENTIALS_BASE64` de desteklenir.

`APP_ENV=production` kullanıldığında güvenlik nedeniyle `OTP_MODE=development` ile servis başlamaz.

## Yerel çalıştırma

Go sürümü `go.mod` dosyasında tanımlıdır.

```bash
cp .env.example .env
# .env içindeki tüm yer tutucuları yalnızca yerel değerlerle değiştirin.
set -a
. ./.env
set +a
go run ./cmd/api
```

Sağlık kontrolü:

```bash
curl http://127.0.0.1:3000/health
```

## Docker ile dağıtım

```bash
docker build -t frame-social-api .
docker run --rm -p 3000:3000 --env-file /secure/path/frame-social.env frame-social-api
```

Render dağıtımında `PORT` değerini elle eklemeyin. Render bu değeri otomatik sağlar ve servis `0.0.0.0:$PORT` üzerinde dinler.

Ortam dosyasını backend klasöründe veya Git deposunda tutmayın. Reverse proxy üzerinden yalnızca HTTPS yayınlayın; SurrealDB portunu internete doğrudan açmayın.

## Kontroller

```bash
gofmt -w .
go vet ./...
go test ./...
```

GitHub Actions her push ve pull request için format, vet ve test kontrollerini çalıştırır.

## API grupları

- `/v1/auth/*`: kayıt, giriş, OTP, token yenileme ve çıkış
- `/v1/me*`: profil ve gizlilik
- `/v1/users/search`, `/v1/friends/*`: arama ve arkadaşlık
- `/v1/conversations/*`, `/v1/messages/*`: sohbet, mesaj, yanıt, tepki, kaydetme ve okundu bilgisi
- `/v1/me/push-devices`: FCM cihaz token yönetimi

Korunan uçlar `Authorization: Bearer <accessToken>` başlığı ister.

## Push Bildirimleri (FCM)

Bir kullanıcı mesaj gönderdiğinde (`POST /v1/conversations/{id}/messages`), mesaj veritabanına kaydedildikten sonra alıcının kayıtlı tüm cihazlarına FCM push gönderilir. Gönderene bildirim gitmez. Push hatası mesaj gönderimini başarısız kılmaz; hata loglanır ve FCM'in geçersiz/not-registered bildirdiği tokenlar silinir.

Push payload:

```json
{
  "notification": { "title": "<gönderen görünen adı>", "body": "Yeni bir mesajın var" },
  "data": {
    "type": "new_message",
    "conversationId": "conversation:xxx",
    "messageId": "message:xxx",
    "senderId": "account:xxx"
  }
}
```

### PUT /v1/me/push-devices

Oturum gerektirir. Bir cihazın FCM tokenını kaydeder veya günceller (upsert). Bir kullanıcı birden fazla cihaz kaydedebilir; eşsiz anahtar `(account, deviceId)`.

İstek:

```http
PUT /v1/me/push-devices
Authorization: Bearer <accessToken>
Content-Type: application/json

{
  "token": "FCM_TOKEN_STRING",
  "platform": "ios",
  "deviceId": "unique-device-id-12345678"
}
```

- `token`: 10-4096 karakter, FCM registration token. Zorunlu.
- `platform`: `ios`, `android` veya `web`. Zorunlu.
- `deviceId`: 8-200 karakter, cihaz benzersiz kimliği. Zorunlu.

Yanıt `200 OK`:

```json
{
  "deviceId": "unique-device-id-12345678",
  "platform": "ios",
  "token": "FCM_TOKEN_STRING",
  "createdAt": "2026-01-01T00:00:00.000000000Z",
  "updatedAt": "2026-01-01T00:00:00.000000000Z"
}
```

Hatalar: `400 invalid_token`, `400 invalid_platform`, `400 invalid_device`, `401 unauthorized`, `429 rate_limited`.

### DELETE /v1/me/push-devices/{deviceId}

Oturum gerektirir. Belirtilen cihaz kaydını siler (idempotent).

```http
DELETE /v1/me/push-devices/unique-device-id-12345678
Authorization: Bearer <accessToken>
```

Yanıt `204 No Content`. Hatalar: `400 invalid_device`, `401 unauthorized`.

### Ortam değişkenleri

`FIREBASE_SERVICE_ACCOUNT_JSON` veya `FIREBASE_SERVICE_ACCOUNT_BASE64` ile Firebase Admin SDK başlatılır. Sırları `.env` veya sunucunun secret yönetimine koyun, Git'e veya Docker imajına eklemeyin. Örnek üretim kurulumu:

```bash
export FIREBASE_PROJECT_ID="my-project-123"
export FIREBASE_SERVICE_ACCOUNT_JSON='{"type":"service_account","project_id":"my-project-123",...}'
# veya
export FIREBASE_SERVICE_ACCOUNT_BASE64="$(base64 -w 0 service-account.json)"
```
