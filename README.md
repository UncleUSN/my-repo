# Ferry

Ferry передаёт файлы между Windows и Linux. Один и тот же бинарник работает как агент (демон) и как CLI. Его можно вызывать из GitLab CI, повесить на расписание или на появление файла в каталоге.

Агент слушает HTTPS, проверяет токен и пишет файл только внутрь каталогов из `allowed_roots`. Перед заменой файла сверяется SHA-256. Если связь оборвалась, повторная команда `send` продолжает загрузку с того места, которое сервер уже записал.

## Как это устроено

На каждой машине запускается один процесс `ferry serve`. У процесса есть:

- HTTPS API (приём и отдача файлов, список заданий, история);
- планировщик заданий из YAML: cron и наблюдение за каталогом;
- список соседних узлов (`peers`), куда он умеет отправлять файлы.

Команды `send`, `get`, `ls` и `ping` сами являются клиентом: локальный демон для них не нужен. Так удобно звать передачу из GitLab CI. Команды `jobs` и `transfers` ходят уже в запущенный локальный агент.

Путь назначения — это строка для узла-получателя, а не для отправителя. Агент в WSL может собрать путь `C:\ferry\inbox\a.txt` и отдать его узлу Windows. Узел Windows примет и `C:\...`, и `C:/...`. Узел Linux примет `/home/...` и отвергнет путь с буквой диска: это ошибка конфигурации, а не файл со странным именем.

```
Windows (ferry.exe :7420)  --HTTPS-->  WSL (ferry :7421)
        ^                                      |
        +---------------- HTTPS ---------------+
```

## Сборка

Нужен Go 1.22 или новее. CGO выключен: на целевой машине ставить Go не нужно, достаточно одного файла.

Linux или WSL:

```bash
make test
make build          # ./bin/ferry
make cross          # dist/ferry-linux-amd64, dist/ferry-linux-arm64, dist/ferry-windows-amd64.exe
make demo           # два узла на 127.0.0.1, передача в обе стороны и watch
```

Windows (PowerShell), если Go стоит только там:

```powershell
go test ./...
.\scripts\build.ps1
```

`build.ps1` кладёт в `dist\` бинарник для Windows и оба бинарника для Linux. Их можно скопировать на сервер без компилятора.

GitHub Actions (`.github/workflows/ci.yml`) гоняет тесты на Ubuntu и Windows и собирает те же три бинарника. Готовые файлы лежат в артефактах сборки.

## Быстрый старт: Windows и WSL на одной машине

Это первая схема, под которую писался пример: Windows-узел и Linux-узел внутри WSL2. Репозиторий удобно держать на диске Windows, например `C:\src\my-repo` — из WSL он виден как `/mnt/c/src/my-repo`. Сами каталоги обмена лучше держать на «родной» файловой системе каждого узла: `C:\ferry` и `~/ferry`, а не гонять агент с бинарником на `/mnt/c` (так медленнее).

Порты специально разные:

| Узел | Слушает | Куда ходит сосед |
| --- | --- | --- |
| Windows | `0.0.0.0:7420` | `https://127.0.0.1:7421` (это WSL) |
| WSL | `0.0.0.0:7421` | `https://<IP Windows>:7420` |

Почему не один порт. В обычном режиме WSL2 (NAT) адрес `127.0.0.1` внутри Windows и внутри WSL — это разные машины. Если процесс Windows занял `0.0.0.0:7420`, то `127.0.0.1:7420` с Windows попадает в него, а не пробрасывается в WSL. Поэтому WSL сидит на **7421**: Windows достучится до него через localhost forwarding (`127.0.0.1:7421`), а WSL до Windows — по IP хоста и порту **7420**.

В WSL `127.0.0.1` — это сам Linux, не Windows. IP Windows со стороны WSL — шлюз по умолчанию. Скрипт `examples/start-wsl.sh` подставляет его в переменную `WIN_HOST`.

Если в `%UserProfile%\.wslconfig` включён `networkingMode=mirrored`, localhost общий, но порты всё равно нельзя занимать дважды. В этом режиме, если адрес шлюза не пингуется, задайте `WIN_HOST=127.0.0.1`.

### 1. Собрать оба бинарника

Из WSL (одной установки Go хватает):

```bash
cd /mnt/c/src/my-repo
make cross
```

Либо из PowerShell на Windows: `.\scripts\build.ps1`.

### 2. Каталоги

PowerShell:

```powershell
New-Item -ItemType Directory -Force -Path C:\ferry\certs, C:\ferry\outbox, C:\ferry\inbox, C:\ferry\archive, C:\ferry\sent, C:\ferry\manual, C:\ferry\data | Out-Null
Copy-Item .\dist\ferry-windows-amd64.exe C:\ferry\ferry.exe
Copy-Item .\examples\start-windows.ps1 C:\ferry\
Copy-Item .\examples\ferry.windows.yaml C:\ferry\ferry.yaml
Copy-Item .\examples\scenarios.windows.yaml C:\ferry\scenarios.windows.yaml
```

WSL:

```bash
mkdir -p ~/ferry/certs ~/ferry/outbox ~/ferry/inbox ~/ferry/archive ~/ferry/sent ~/ferry/manual ~/ferry/data
cp /mnt/c/src/my-repo/dist/ferry-linux-amd64 ~/ferry/ferry
cp /mnt/c/src/my-repo/examples/start-wsl.sh ~/ferry/
cp /mnt/c/src/my-repo/examples/ferry.wsl.yaml ~/ferry/ferry.yaml
cp /mnt/c/src/my-repo/examples/scenarios.wsl.yaml ~/ferry/scenarios.wsl.yaml
chmod +x ~/ferry/ferry ~/ferry/start-wsl.sh
```

Дальше в обоих `ferry.yaml` замените токен-заглушку `change-me-to-a-long-random-token` на один и тот же длинный секрет и подставьте имя пользователя вместо `USER` в путях WSL (`/home/USER/...` → `/home/ваше_имя/...`).

Секрет, например:

```powershell
# PowerShell
-join ((1..32 | ForEach-Object { '{0:x2}' -f (Get-Random -Max 256) }))
```

```bash
openssl rand -hex 24
```

Токен в `node.token` — это пароль **этого** узла. Токен в `peers[].token` — пароль **соседа**. В этой схеме они одинаковые, так проще сверить глазами.

### 3. Сертификаты

Связь только по TLS. Для лаборатории проверка имени сертификата выключена (`tls_skip_verify: true`): IP WSL меняется после перезапуска, и привязка к имени только мешает. Токен при этом всё равно передаётся внутри TLS, просто клиент не проверяет, чей это сертификат. В интернет так не выставляйте. Как включить проверку — в конце раздела.

PowerShell:

```powershell
C:\ferry\ferry.exe cert --dir C:\ferry\certs --cn win
```

WSL:

```bash
~/ferry/ferry cert --dir ~/ferry/certs --cn wsl
```

`localhost` и `127.0.0.1` добавляются сами: они нужны, чтобы CLI мог говорить с локальным агентом.

### 4. Брандмауэр Windows

Из WSL пакет до Windows идёт как входящее подключение и по умолчанию часто режется. Правило нужно для порта **7420** (агент Windows). Порт 7421 слушает WSL; с Windows до него ходят через localhost forwarding, отдельное входящее правило обычно не требуется.

PowerShell **от администратора**:

```powershell
New-NetFirewallRule -DisplayName "Ferry Windows agent 7420" -Direction Inbound -Action Allow -Protocol TCP -LocalPort 7420
```

Правило без `-Profile` действует на все профили. Виртуальный адаптер WSL иногда числится как Public, и правило только для Private его не покроет.

Агент слушает `0.0.0.0`, то есть порт открыт и для локальной сети, не только для WSL. Токен это единственный пароль. Не используйте заглушку из примера и не пробрасывайте порт в интернет.

### 5. Запустить оба узла

Сначала WSL — скрипт печатает `WIN_HOST` и не вернётся, пока агент работает (второе окно или вкладка терминала):

```bash
cd ~/ferry
./start-wsl.sh
```

В строке должно быть что-то вроде `агент запущен` и `listen=0.0.0.0:7421`. Предупреждение про `tls_skip_verify` и про токен `change-me` (если вы его ещё не заменили) — ожидаемо.

Затем Windows:

```powershell
cd C:\ferry
.\start-windows.ps1
```

Остановка в обоих окнах — Ctrl+C. Журнал ещё пишется в `data\ferry.log` и `~/ferry/data/ferry.log`.

### 6. Проверить, что узлы видят друг друга

Новое окно PowerShell:

```powershell
C:\ferry\ferry.exe ping --config C:\ferry\ferry.yaml --peer wsl
```

Ожидается: `узел wsl отвечает (id=wsl)`.

WSL:

```bash
~/ferry/ferry ping --config ~/ferry/ferry.yaml --peer win
```

Ожидается: `узел win отвечает (id=win)`.

Если WSL не достучался до Windows — почти всегда брандмауэр или неверный `WIN_HOST`. Если Windows не достучался до WSL:

- в WSL процесс слушает `0.0.0.0:7421`, а не `127.0.0.1`;
- порт 7421 на Windows никто не занял (`Get-NetTCPConnection -LocalPort 7421 -State Listen` должен быть пустым);
- после сна localhost forwarding иногда отваливается: `wsl --shutdown` и открыть дистрибутив заново;
- в `.wslconfig` не выключен `localhostForwarding` (по умолчанию он включён).

Пишите `127.0.0.1`, не `localhost`: имя иногда раскрывается в `::1`, а проброс WSL — IPv4.

### 7. Файл Windows → WSL

```powershell
New-Item -ItemType Directory -Force -Path C:\ferry\manual | Out-Null
[System.IO.File]::WriteAllText('C:\ferry\manual\from-windows.txt', "hello from windows`n")
C:\ferry\ferry.exe send --config C:\ferry\ferry.yaml --peer wsl `
  --src C:\ferry\manual\from-windows.txt `
  --dst /home/USER/ferry/inbox/from-windows.txt
```

Подставьте свой домашний каталог. В WSL:

```bash
cat ~/ferry/inbox/from-windows.txt
```

### 8. Файл WSL → Windows

На узле Windows путь можно передать и с обратными слэшами, и с обычными. Из bash удобнее второй вариант: кавычки не съедают `\`.

```bash
echo 'hello from wsl' > ~/ferry/manual/from-wsl.txt
~/ferry/ferry send --config ~/ferry/ferry.yaml --peer win \
  --src ~/ferry/manual/from-wsl.txt \
  --dst 'C:/ferry/inbox/from-wsl.txt'
```

PowerShell:

```powershell
Get-Content C:\ferry\inbox\from-wsl.txt
```

`/mnt/c/...` здесь не участвует. Это был бы путь диска Windows, видимый из Linux, а не передача по сети. Агент в WSL может писать и туда (если каталог есть в `allowed_roots`), но для проверки сети используйте `~/ferry` и `C:\ferry`.

### 9. Задание «появился файл»

Сценарий `watch-to-wsl` в `C:\ferry\scenarios.windows.yaml` смотрит на `C:\ferry\outbox`. Когда размер файла не меняется `settle` (в примере 2 секунды), агент кладёт копию в две папки WSL — alias `inbox` и alias `archive` — и только после обеих успешных доставок переносит исходник в `C:\ferry\sent`. Опрос, а не inotify: одинаково работает на Windows и в WSL и не подхватывает файл, который ещё дописывается.

Оба `serve` должны быть запущены.

```powershell
[System.IO.File]::WriteAllText('C:\ferry\outbox\auto.txt', "via watch`n")
```

Через несколько секунд:

```bash
cat ~/ferry/inbox/auto.txt ~/ferry/archive/auto.txt
```

```powershell
Get-Content C:\ferry\sent\auto.txt
Test-Path C:\ferry\outbox\auto.txt   # False
```

История на каждом узле:

```powershell
C:\ferry\ferry.exe transfers --config C:\ferry\ferry.yaml
```

```bash
~/ferry/ferry transfers --config ~/ferry/ferry.yaml
```

На отправителе направление `out`, на получателе после завершённой загрузки — `in`.

Тот же сценарий двумя процессами на одном Linux проверяет `make demo` (`scripts/demo-two-nodes.sh`).

### Если что-то не так

| Симптом | Куда смотреть |
| --- | --- |
| `URL has an empty host` | В WSL не задан `WIN_HOST`. Запускайте через `start-wsl.sh`. |
| `connection refused` на 7421 | Агент WSL не запущен или слушает другой порт. |
| таймаут на 7420 из WSL | Брандмауэр Windows или неверный IP хоста. |
| `unauthorized` | Токены в двух файлах различаются. |
| `outside allowed_roots` | Путь назначения не внутри `allowed_roots` **того узла, который файл принимает**. |
| `Windows path ... cannot be used` | В `--dst` для узла WSL попал путь `C:\...`. Для WSL нужен путь Linux. |
| watch молчит | Файл ещё растёт (`settle`), шаблон `pattern` не совпал, или `archive_dir` лежит внутри каталога наблюдения. |
| `change-me` в журнале | Вы не заменили токен из примера. |

### Проверка сертификата вместо tls_skip_verify

Скопируйте `cert.pem` соседнего узла и укажите его как `ca_file`, а `tls_skip_verify` уберите (или поставьте `false`). В сертификат Windows добавьте IP, по которому ходит WSL:

```powershell
C:\ferry\ferry.exe cert --dir C:\ferry\certs --cn win --ip 172.31.0.1
```

IP возьмите из строки `WIN_HOST=...`. После смены сертификата агент надо перезапустить. Для GitLab CI так и стоит делать: в репозиторий кладётся только сертификат сервера, не ключ.

## Команды

```text
ferry serve --config ferry.yaml
ferry send --config ferry.yaml --peer <узел> --src <локальный файл> --dst <путь на узле>
ferry get  --config ferry.yaml --peer <узел> --src <путь на узле> --dst <локальный файл>
ferry ls   --config ferry.yaml --peer <узел> --path <каталог>
ferry ping --config ferry.yaml --peer <узел>
ferry jobs list --config ferry.yaml
ferry jobs run --config ferry.yaml <имя>
ferry transfers --config ferry.yaml [--limit 20]
ferry status --config ferry.yaml
ferry config validate --config ferry.yaml [--scenarios файл]
ferry scenarios reload --config ferry.yaml
ferry alerts tail --config ferry.yaml [--level error] [--follow]
ferry cert --dir <каталог> [--host имя]... [--ip адрес]...
ferry service install|uninstall|start|stop|restart --config ferry.yaml
ferry version
```

Флаги пишите до имени задания. `--json` у `send`, `get`, `ls`, `ping`, `jobs` и `transfers` печатает JSON. Путь к конфигу по умолчанию — `ferry.yaml` или переменная `FERRY_CONFIG`. В значениях YAML раскрывается `${ПЕРЕМЕННАЯ}`.

Код выхода: `0` — успех, `1` — ошибка передачи или задания, `2` — неверные аргументы.

## Конфигурация

Конфиг узла (`ferry.yaml`) и сценарии (`scenarios.yaml` или каталог `scenarios.d/`) разделены. В узле — кто слушает, соседи, входящие папки. В сценариях — откуда брать файлы и куда их раскладывать. Относительные пути считаются от каталога своего YAML, а не от текущей папки процесса.

Проверка без запуска: `ferry config validate --config ferry.yaml`. Перечитать сценарии у живого агента: `ferry scenarios reload` или `kill -HUP <pid>` (на Windows — только команда или `POST /api/v1/scenarios/reload`). Порт и сертификат при этом не перезапускаются.

Полные примеры: `examples/ferry.windows.yaml`, `examples/scenarios.windows.yaml`, `examples/ferry.wsl.yaml`, `examples/scenarios.wsl.yaml`.

Старый блок `jobs:` в `ferry.yaml` по-прежнему работает: каждое задание становится сценарием с одним источником и одним назначением. Новые сценарии лучше держать отдельно.

```yaml
# ferry.yaml — узел и реестр соседей
node:
  id: win
  listen: "0.0.0.0:7420"
  token: "${FERRY_TOKEN}"
  data_dir: "C:\\ferry\\data"
  scenarios: "C:\\ferry\\scenarios.yaml"   # файл или каталог scenarios.d
  allowed_roots:
    - "C:\\ferry"
  inbound:                                  # отправители целятся в alias, не в сырой путь
    - name: inbox
      path: "C:\\ferry\\inbox"
    - name: archive
      path: "C:\\ferry\\archive"
  tls:
    cert_file: "C:\\ferry\\certs\\cert.pem"
    key_file: "C:\\ferry\\certs\\key.pem"

peers:
  - id: wsl
    url: "https://127.0.0.1:7421"
    token_file: "C:\\ferry\\secrets\\wsl.token"  # либо token: "${WSL_TOKEN}"
    tls_skip_verify: true

# scenarios.yaml — что передавать
scenarios:
  - name: watch-to-wsl
    watch: true                          # опрос sources
    schedule: "15 2 * * *"               # можно вместе с watch; пять полей или @every
    settle: 2s
    interval: 2s
    sources:
      - path: "C:\\ferry\\outbox"
        include: ["*"]                   # по умолчанию *
        exclude: ["*.tmp"]
        recursive: false
      - path: "C:\\ferry\\reports"
        include: ["*.csv"]
    destinations:                        # веер: N узлов и/или N папок
      - peer: wsl
        alias: inbox                     # папка на получателе
      - peer: wsl
        alias: archive
      - peer: wsl
        alias: inbox
        path: reports                    # подкаталог внутри alias
    on_success: move                     # keep | none | delete | move
    success_policy: all                  # all — все назначения; any — хотя бы одно, остальные в финальном состоянии
    archive_dir: "C:\\ferry\\sent"
    retry:
      max_attempts: 5
      initial_backoff: 2s
      max_backoff: 1m
```

Правила:

- `success_policy: all` переносит или удаляет исходник, только когда каждое назначение подтвердило файл. `any` — когда хотя бы одно подтвердило и остальные уже не ждут повтора (доставлены или исчерпали попытки).
- Повтор каждой пары «файл, назначение» независим. Состояние лежит в `data_dir/deliveries.db` (bbolt): после перезапуска доставленное с тем же SHA-256 не уходит снова, а незавершённое продолжается.
- Несколько источников в одном сценарии кладут файлы в подкаталоги по имени папки (`outbox/a.txt` и `reports/a.txt`), чтобы имена не затёрли друг друга. Один источник сохраняет прежнее поведение: в назначении оказывается сам относительный путь.
- Alias проверяется на получателе и всё равно обязан лежать в `allowed_roots`. Абсолютный путь вместе с alias отвергается.
- Неизвестное поле в YAML — ошибка при загрузке.

Ручной запуск: `ferry jobs run --config ferry.yaml имя` или `POST /api/v1/jobs/имя/run`. Сводка: `ferry status`.

События доставки (`queued`, `started`, `delivered`, `failed`, `retried`: файл, размер, хеш, сценарий, назначение, попытка, длительность) публикуются во внутреннюю шину. Эндпоинт Prometheus к ней ещё не подключён — это следующий шаг. Читать текущее состояние можно уже сейчас: `GET /api/v1/deliveries`.

## Мониторинг

Все события пишутся в `alerts.log` (JSON, одна строка — одно событие). Путь по умолчанию — `data_dir/alerts.log`. Передача файла не ждёт диск: если буфер полон, строка пропускается и в журнал попадает `alerts_dropped`.

```yaml
monitoring:
  delivery_timeout: 15m          # сколько ждать пару «файл, назначение», прежде чем писать delivery_overdue
  alerts_log:
    path: "C:\\ferry\\data\\alerts.log"
    min_level: info              # info | warn | error
    max_size_mb: 10              # после этого файл становится alerts.log.1
    max_backups: 5
    max_age_days: 14
```

Уровни: обычные события (`queued`, `started`, `delivered`, старт и остановка службы, успешная перечитка сценариев) — `info`. Повтор и просрочка (`retried`, `delivery_overdue`, сосед недоступен) — `warn`. Исчерпание попыток (`delivery_failed`), ошибка конфига и ошибка обхода каталога — `error`.

Пример строк:

```json
{"timestamp":"2026-11-11T22:15:01.120+03:00","level":"info","event":"delivered","node":"win","scenario":"watch-to-wsl","source":"C:\\ferry\\outbox\\auto.txt","destination":"wsl:inbox/auto.txt","peer":"wsl","size":11,"sha256":"ab12","attempt":1,"duration_ms":18}
{"timestamp":"2026-11-11T22:15:04.200+03:00","level":"warn","event":"retried","node":"win","scenario":"watch-to-wsl","source":"C:\\ferry\\outbox\\auto.txt","destination":"wsl:archive/auto.txt","peer":"wsl","attempt":2,"duration_ms":5,"error":"connection refused"}
{"timestamp":"2026-11-11T22:30:01.000+03:00","level":"warn","event":"delivery_overdue","node":"win","scenario":"watch-to-wsl","source":"C:\\ferry\\outbox\\auto.txt","destination":"wsl:archive/auto.txt","peer":"wsl","error":"delivery is still not finished"}
{"timestamp":"2026-11-11T22:31:10.000+03:00","level":"error","event":"delivery_failed","node":"win","scenario":"watch-to-wsl","source":"C:\\ferry\\outbox\\auto.txt","destination":"wsl:archive/auto.txt","peer":"wsl","attempt":5,"error":"connection refused"}
```

Смотреть файл:

```powershell
Get-Content -Wait C:\ferry\data\alerts.log
C:\ferry\ferry.exe alerts tail --config C:\ferry\ferry.yaml --level error --follow
```

```bash
tail -f ~/ferry/data/alerts.log
~/ferry/ferry alerts tail --config ~/ferry/ferry.yaml --level warn --follow
```

Шина событий уже отделена от журнала. Позже к ней можно подключить Prometheus (`/metrics`) и внешние уведомления: для этого в коде есть интерфейсы `Metrics` и `Alerter`, реализации пока нет.

## HTTP API

Все методы, кроме `GET /api/v1/health`, требуют заголовок `Authorization: Bearer <token>` или `X-Ferry-Token`.

| Метод | Путь | Назначение |
| --- | --- | --- |
| GET | `/api/v1/health` | жив ли порт, без токена |
| GET | `/api/v1/whoami` | проверка токена |
| POST | `/api/v1/uploads` | начать загрузку (`path`, `alias`, `size`, `sha256`, `overwrite`) |
| GET | `/api/v1/deliveries?scenario=&status=&limit=` | состояние по каждому назначению |
| POST | `/api/v1/scenarios/reload` | перечитать сценарии без рестарта |
| PUT | `/api/v1/uploads/{id}` | кусок файла, заголовок `X-Offset` |
| POST | `/api/v1/uploads/{id}/commit` | сверить хеш и положить файл на место |
| GET | `/api/v1/uploads/{id}` | текущее смещение |
| GET | `/api/v1/fs/checksum?path=` | SHA-256 и размер |
| GET | `/api/v1/fs/download?path=` | скачать, заголовок `Range: bytes=START-` для докачки |
| GET | `/api/v1/fs/list?path=` | каталог |
| GET | `/api/v1/jobs` | задания |
| POST | `/api/v1/jobs/{name}/run` | запустить задание и дождаться конца |
| GET | `/api/v1/transfers?limit=` | история, новые сверху |

Если в `jobs/{name}/run` часть файлов не ушла, код ответа 500, а в теле всё равно счётчики `transferred` и `failed`.

## Служба

### Windows

Не регистрируйте `ferry.exe` через `sc.exe create` как обычную программу. Диспетчер служб завершит её с ошибкой 1053: процесс не отвечает на команды службы. Бинарник умеет это сам.

PowerShell от администратора:

```powershell
C:\ferry\ferry.exe service install --config C:\ferry\ferry.yaml
C:\ferry\ferry.exe service start
C:\ferry\ferry.exe service stop
C:\ferry\ferry.exe service uninstall
```

Тот же набор команд есть в `deploy/install-windows.ps1`. Конфиг и `data_dir` — с абсолютными путями: служба стартует не из `C:\ferry`. Журнал — `C:\ferry\data\ferry.log`. В консоли для проверки удобнее `.\start-windows.ps1`, служба — когда окно больше не нужно.

### Linux (systemd)

Юнит целиком: `examples/ferry.service`. Установка от root: `sudo bash deploy/install-linux.sh` (кладёт бинарник в `/usr/local/bin/ferry`, ждёт конфиг в `/etc/ferry/ferry.yaml`, пользователь `ferry`, данные в `/var/lib/ferry`).

Вручную:

```bash
sudo install -m 0755 dist/ferry-linux-amd64 /usr/local/bin/ferry
sudo install -d -m 0750 /etc/ferry /var/lib/ferry
sudo install -m 0644 examples/ferry.service /etc/systemd/system/ferry.service
sudo systemctl daemon-reload
sudo systemctl enable --now ferry
sudo systemctl status ferry
```

Команда `ferry service install` делает то же через библиотеку служб (на Linux это systemd, если он есть). Юнит выше проще прочитать глазами, для сервера лучше он.

В WSL для первого прогона служба не нужна: два терминала и Ctrl+C. Если нужен systemd внутри WSL, в `/etc/wsl.conf` должно быть `systemd=true`, затем `wsl --shutdown`.

## GitLab CI

На раннере нужен только бинарник и конфиг без локального `serve`. Токен сервера положите в CI/CD Variables как masked `FERRY_TOKEN`, в конфиге напишите `token: "${FERRY_TOKEN}"`. Сертификат сервера (не ключ) укажите в `ca_file`.

```yaml
transfer_artifact:
  stage: deploy
  script:
    - ./ferry send --config ferry-ci.yaml --peer prod --src dist/app.zip --dst /opt/incoming/app.zip
```

Фрагмент лежит в `examples/gitlab-ci.yml`. `--json` удобно, если следующий шаг разбирает ответ.

Минимальный `ferry-ci.yaml`:

```yaml
node:
  id: gitlab-runner
  token: "runner-does-not-listen"
peers:
  - id: prod
    url: "https://prod.example:7420"
    token: "${FERRY_TOKEN}"
    ca_file: "certs/prod.pem"
```

## Безопасность

- Без TLS агент не стартует и клиент не ходит на `http://`.
- Пустой `allowed_roots` разрешает любой локальный путь. В примерах список всегда задан.
- Символическая ссылка, ведущая наружу из `allowed_roots`, отвергается. Задания ссылки не передают.
- `tls_skip_verify: true` — только для Windows+WSL на своей машине.
- Порт на `0.0.0.0` виден сети. Ограничьте его брандмауэром.
- Не коммитьте настоящий токен и `key.pem`.

## Ограничения этой версии

- Наблюдение за каталогом — опрос с периодом `interval`, не inotify/ReadDirectoryChanges. Смена содержимого без смены размера и mtime может быть не замечена, пока файл не тронут ещё раз.
- История передач — JSON-файл, последние 500 записей. Журнал доставки по назначениям — отдельная база bbolt.
- Метрики Prometheus ещё не отдаются: есть шина событий и `GET /api/v1/deliveries`.
- Один токен на узел, без пользователей и ролей.
- Файлы внутри одного задания отправляются по очереди.
- Время изменения и права на стороне получателя не сохраняются, файл создаётся с правами `0644`.
- Веб-интерфейса нет: CLI, API и журнал.
