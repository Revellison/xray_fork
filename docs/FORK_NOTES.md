# Форк Xray-core: Шейпер и расширенный sniffing

Этот форк добавляет в Xray-core:

1. **Шейпер скорости per-user (по email)**
   - Раздельные лимиты download/upload в kbps
   - Лесенка штрафов с независимыми down/up секциями
   - Конфиг в секции `speedLimit` файла `config.json`
   - Token bucket без резервации вперёд, минимальная задержка
   - UDP: дроп по таймауту вместо блокировки потока

2. **Расширенный sniffing из Jolymmiles/Xray-core**
   - QUIC v2 Initial packets
   - Сборка ClientHello из нескольких QUIC Initial пакетов
   - Сборка TLS ClientHello из нескольких записей
   - BitTorrent: uTP, DHT (KRPC), UDP tracker
   - Датаграммы форвардятся без изменений

## Изменённые файлы

### Шейпер (Часть A)

Новые файлы:
- `app/speedlimit/config.go` — типы конфига
- `app/speedlimit/validate.go` — разбор и валидация (V01–V27)
- `app/speedlimit/bucket.go` — token bucket
- `app/speedlimit/ring.go` — скользящее окно (60 корзин)
- `app/speedlimit/ladder.go` — машина состояний штрафа
- `app/speedlimit/manager.go` — реестр пользователей, тикер
- `app/speedlimit/embedded.go` — установка конфига из `config.json`
- `infra/conf/speedlimit.go` — разбор секции `speedLimit`
- `app/speedlimit/writer.go` — обёртка buf.Writer
- `app/speedlimit/clock.go` — интерфейс времени (для тестов)

Изменённые файлы:
- `app/dispatcher/default.go` — интеграция WrapUp/WrapDown после SizeStatWriter
- `infra/conf/xray.go` — поле `speedLimit` и его сборка

### Sniffing (Часть B)

Перенесено из Jolymmiles/Xray-core (commit 15f6179):
- `common/protocol/protocol.go` — ErrProtoNeedMoreData
- `common/protocol/tls/sniff.go` — reassembleClientHello, HandshakeMessageLength
- `common/protocol/bittorrent/bittorrent.go` — SniffUTP
- `common/protocol/bittorrent/dht.go` — KRPC (Mainline DHT)
- `common/protocol/bittorrent/udp_tracker.go` — BEP 15
- `common/protocol/quic/sniff.go` — v1/v2, сборка из Initial
- `common/protocol/quic/qtls_go118.go` — доступ к aeadAESGCMTLS13
- `app/dispatcher/sniffer.go` — SniffDatagrams, реестр снифферов
- `app/dispatcher/fakednssniffer.go` — обёртка FakeDNS
- `app/dispatcher/default.go` — cachedReader.Datagrams(), цикл сниффинга

Все тестовые файлы (`*_test.go`) и `testdata/` также перенесены.

## Настройка шейпера

Секция `speedLimit` находится в корне `config.json`, рядом с `inbounds` и `outbounds`. Если секции нет, шейпер выключен (нулевая стоимость). Лимиты задаются в кбит/с: `0` означает безлимит, ненулевой минимум равен 64.

```json
{
  "inbounds": [],
  "outbounds": [],
  "speedLimit": {
    "enabled": true,
    "penalties": true,
    "default": { "down": 50000, "up": 20000, "penalty": "standard" },
    "profiles": {
      "standard": {
        "down": {
          "steps": [{ "above": 45000, "for": "5m", "limit": 25000 }],
          "recover": { "after": "2h" }
        },
        "up": {
          "steps": [{ "above": 18000, "for": "3m", "limit": 8000 }],
          "recover": { "after": "1h" }
        }
      }
    },
    "users": [
      { "emails": ["vip@example"], "down": 100000, "up": 100000, "penalty": "none" }
    ]
  }
}
```

Лимиты работают для всех входящих протоколов и транспортов: VLESS (raw, Vision, WebSocket, XHTTP, gRPC), Hysteria 2, Trojan, VMess, Shadowsocks. При лимите на отправку `mux` лучше выключить: у него нет управления потоком на уровне отдельных соединений, и ограниченная отправка задерживает остальные TCP-соединения в том же туннеле.

Лимиты применяются к пользователям по их `email` во входящих подключениях. Ошибка в секции `speedLimit` не даёт ядру запуститься. Проверка конфига: `xray run -test -c config.json`.

## Конфигурация sniffing

В конфиге Xray включите sniffing для UDP:

```json
{
  "inbounds": [{
    "sniffing": {
      "enabled": true,
      "destOverride": ["quic", "bittorrent"]
    }
  }]
}
```

Датаграммы форвардятся без изменений; sniffing работает из кеша dispatcher.

## Upstream-синхронизация

Порядок обновления, разрешение конфликтов и выпуск релизов описаны в [FORK_MAINTENANCE.md](../FORK_MAINTENANCE.md).

## Проверка работоспособности

### Сборка

```bash
go build ./...
```

### Тесты шейпера

```bash
go test ./app/speedlimit/... -v
```

### Тесты sniffing

```bash
go test ./app/dispatcher/... -race -v
go test ./common/protocol/quic/... -race
go test ./common/protocol/tls/... -race
go test ./common/protocol/bittorrent/... -race
```

## Лицензия

Этот форк основан на Xray-core (MPL 2.0). Расширенный sniffing перенесён из Jolymmiles/Xray-core. Шейпер написан для этого форка, MPL 2.0. Происхождение и лицензии тестовых данных указаны в файлах `PROVENANCE.md` и `LICENSE-MIT` в каталогах `testdata/`.
