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

Korunan uçlar `Authorization: Bearer <accessToken>` başlığı ister.
