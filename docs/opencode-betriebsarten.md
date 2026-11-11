# opencode: Betriebsarten

Stand: 2026-11-11. Getestet wurde `opencode v2.0.29` (macOS, arm64).
Neueste Version zum Stand: v2.2.7 (Release 2026-11-10). Die v1-Linie wird parallel weiter
veröffentlicht (letzter geprüfter Tag: v1.19.5, 2026-10-28).

Kennzeichnung jeder Aussage:

- **[getestet]** lokal gegen v2.0.29 ausgeführt (isolierte Umgebung, nur localhost)
- **[Quelle]** aus dem Quellcode gelesen (Tags v2.0.29, v2.2.7, v1.19.5 in `anomalyco/opencode`)
- **[Doku]** aus offizieller Doku oder Drittseiten gelesen
- **[ungeprüft]** weder getestet noch im Code belegt

## 0. Wichtigster Befund: es gibt zwei CLI-Linien

| | v1 (Default-Installer) | v2 (`/v2/install`) |
|---|---|---|
| Installer | `https://opencode.ai/install` leitet auf `raw.githubusercontent.com/anomalyco/opencode/refs/heads/v1/install` weiter [beobachtet per WebFetch; Skript selbst nicht gelesen, "Default = v1" daher Schlussfolgerung] | `https://opencode.ai/v2/install` [Quelle: `install` im v2-Tag, Zeile 25] |
| Version | 1.19.x | 2.x |
| Befehle | `tui`, `serve`, `web`, `attach`, `run`, `github`, `export`, `import`, `acp`, `mcp`; `share`: im Tag v1.19.5 keine Befehlsdatei gefunden, siehe Abschnitt 1 [Quelle] | `serve`, `service`, `pair`, `run`, `mini`, `session export/import`, `api`, `acp`, `mcp`, `auth` [getestet: `--help`] |
| Auth | Basic, Benutzer `OPENCODE_SERVER_USERNAME` (Default `opencode`), Passwort `OPENCODE_SERVER_PASSWORD`; **ohne Passwort läuft der Server offen** (nur eine Warnung) [Quelle: `serve.ts`, `auth.ts` v1.19.5] | Basic, Benutzer fest `opencode`, Passwort `OPENCODE_PASSWORD` (alt: `OPENCODE_SERVER_PASSWORD`); **ohne Passwort wird ein zufälliges erzeugt**, kein Offen-Modus [Quelle: `server-process.ts`, `auth.ts`; getestet] |
| Run gegen Server | `run --attach <url> [--dir <Pfad auf dem Server>] [-p/-u]` [Quelle: `run.ts` v1] | `run --server <url>`, Passwort nur per Env, **kein `--dir`** [getestet, Quelle] |

Die verbreiteten Anleitungen (`web`, `attach`, `run --attach`, `github`, `export`) beschreiben die v1-Linie.
Auf v2 (installiert) existieren `web`, `attach`, `github` und ein Top-Level-`export` **nicht**
[getestet: `opencode web|attach|github --help` zeigt nur die Hilfe der Wurzel; Befehlsliste in v2.2.7 identisch].
Die offizielle Doku unter `opencode.ai/docs` beschreibt v1 und verweist für v2 auf `opencode.ai/v2/docs`
[Doku, 2026-11-11]. Wo die Doku und der v2-Code auseinanderlaufen, gilt hier der Code.

Folge: Wer opencode als Dauerdienst betreibt, muss zuerst die Linie wählen; Befehle und Optionen unterscheiden sich.

## 1. Befehle

### v2 (installiert, v2.0.29)

| Befehl | Verhalten |
|---|---|
| `opencode [dir]` | TUI. Verbindet sich mit dem **gemeinsamen Hintergrunddienst** (`opencode service`), startet ihn bei Bedarf. `--standalone` startet einen privaten Server, `--server <url>` verbindet zu einem fremden [Quelle: `default.ts`, `server-connection.ts`] |
| `opencode serve [--hostname --port --cors <origin>…] [--service] [--stdio]` | Ein Server-Prozess im Vordergrund. **Liefert API und Web-UI aus demselben Port** ("Start the v2 API and web server") [getestet: `/` liefert HTML ohne Auth, `/api/*` verlangt Auth]. Standardmäßig `127.0.0.1`, zufälliger Port ohne `--port`. `--service` ist der Modus des Hintergrunddienstes (zieht Konfig aus `service.json`) |
| `opencode service start/stop/restart/status/get/set/unset` | Hintergrunddienst pro Benutzer (Dateien in `~/.config/opencode/service.json`); `set hostname/port/password/cors/remote/disabled` [Quelle: `service-config.ts`; Doku] |
| `opencode pair [--url <u>] [--remote [--yes]]` | Druckt Einmal-Link und QR für Browser oder App. Braucht den Hintergrunddienst (`ServiceConfig.disabled` darf nicht gesetzt sein). `--remote` schaltet einen OpenTunnel-Zugang ein und **macht den Server im Internet erreichbar** [Quelle: `pair.ts`] |
| `opencode run [msg…]` | Siehe Abschnitt 2. Ist ein **Client**, kein eigener Agent-Prozess |
| `opencode mini` | Minimale interaktive Oberfläche; gleiche Verbindungsoptionen wie `run` |
| `opencode session list/delete/export/import` | Arbeitet gegen den Server (`--server`/`--standalone`). `export [--sanitize]` liefert JSON `{info, messages}` [getestet] |
| `opencode api <operationId \| METHOD pfad>` | Roher Aufruf der HTTP-API des laufenden Servers (`-d`, `-H`, `--param`) [Hilfe] |
| `opencode acp` | Agent Client Protocol über stdio, für Editoren; `--login` [Hilfe; nicht getestet] |
| `opencode mcp list/add/auth/logout`, `opencode plugin …`, `opencode auth list/login/logout/export/import/switch`, `opencode models`, `opencode stats`, `opencode reload`, `opencode debug agents/config/paths` | Verwaltung. `auth export` druckt **gespeicherte Zugangsdaten im Klartext** [Hilfe] |
| `share` | **Kein Befehl und keine HTTP-Route in v2** [Quelle: Routen im `openapi.json`, 139 Endpunkte]. Der Config-Key `share` wird noch geparst (`normalize.ts`) |

### v1 (nicht installiert, nur aus Quellcode v1.19.5)

- `opencode serve`: headless Server. `opencode web`: **derselbe Server plus Browser-Öffnen und Ausgabe der Netzwerk-Adressen**, sonst kein Unterschied
  (`web.ts` und `serve.ts` rufen beide `Server.listen`) [Quelle]. Flags `--port` (Default 4096), `--hostname`, `--mdns`, `--mdns-domain`, `--cors` [Quelle, Doku].
- `opencode attach <url>`: TUI gegen einen laufenden Server. `opencode run --attach <url>`: Prompt gegen einen laufenden Server [Quelle].
- `opencode github install|run`: Hilfsbefehl für die GitHub-Action (siehe Abschnitt 6). `opencode export|import`: Session-JSON. `share`: **widersprüchlich [ungeprüft]**. `cmd/run.ts` v1.19.5 nennt die Option `--share` deprecated ("session sharing was discontinued"), eine Befehlsdatei `share` existiert im Tag nicht; `github/action.yml` und `share.mdx` beschreiben Teilen dagegen als aktiv. Vor dem Einsatz einer v1-Instanz `"share": "disabled"` setzen.

## 2. `opencode run`

Alle Punkte v2 [getestet], sofern nicht anders gekennzeichnet.

- **Client**: baut keine eigene Agentenschleife. Ohne `--server` nutzt es den Hintergrunddienst, mit `--standalone` einen privaten Serverprozess, mit `--server <url>` einen vorhandenen Server. Der Agent läuft immer **im Serverprozess**, nicht im `run`-Prozess.
- Flags: `--session/-s <id>` (legt an, wenn nicht vorhanden), `--continue/-c`, `--fork` (braucht `-c` oder `-s`), `--model/-m provider/model#variant`, `--agent`, `--file/-f` (max. 10 MiB, als Data-URL gesendet), `--title`, `--thinking`, `--format default|json`, `--auto` (Aliase `--yolo`, `--dangerously-skip-permissions`, versteckt) [Hilfe, Quelle]. Stdin wird an die Nachricht angehängt, wenn kein TTY vorliegt.
- `--format json`: eine JSON-Zeile pro Ereignis (`step_start`, `text`, Tool-Teile, am Ende `error` bei Fehlern), je mit `sessionID`. Fehler: `{"type":"error",…}` und Exit-Code 1 [getestet, Quelle].
- **Arbeitsverzeichnis**: `run` sendet sein eigenes `cwd` (`$PWD`) als `location.directory` an den Server. Es gibt in v2 **kein `--dir`**. Bei Client und Server auf derselben Maschine arbeitet der Agent also im Verzeichnis des Aufrufers [getestet]. Ein Client auf einem anderen Host (GitHub-Runner) sendet ein Verzeichnis, das auf dem Server meist nicht existiert; der Server antwortet dann `LocationNotFoundError` [getestet via API, nicht via `run`]. Die v1-Linie hat dafür `--dir` ("Pfad auf dem Server") [Quelle].
- **Permission `ask` ohne Terminal** [getestet, `bash: ask` in der Projekt-`opencode.json`]: `run` meldet `permission requested: shell (…); auto-rejecting`, lehnt die Anfrage ab, der Tool-Aufruf scheitert mit einer Meldung an das Modell, die Sitzung **läuft weiter**, **Exit-Code 0**. CI muss deshalb `--format json` auswerten (Fehlerereignisse) oder das Ergebnis über die Sitzung prüfen. Fragen (`question`-Tool) werden analog abgebrochen [Quelle: `noninteractive.ts`]. In v1 bricht der Lauf nach einer Ablehnung ab [Quelle].
- **`--auto`**: genehmigt alles, was nicht ausdrücklich `deny` ist (Antwort `once`); explizite `deny`-Regeln gelten weiter [Quelle, Doku `permissions.mdx`]. **Nicht ausgeführt** (nur aus Quellcode und Doku).
- **Standard ohne Konfiguration**: Im Test lief `bash` ohne Rückfrage durch (Default-Berechtigung also `allow` in der leeren Umgebung) [getestet]. Eine Rückfrage gibt es erst nach Konfiguration (`permission`).
- Exit-Code 0 auch bei verweigerter Berechtigung; Exit-Code 1 bei Fehlern wie falschem Passwort oder unerreichbarem Server [Quelle: `reportRunError`].
- Versionsabweichung zwischen Client und Server erzeugt nur eine Warnung auf stderr [Quelle: `server-connection.ts`].

## 3. Server

### 3.1 Auth

- HTTP Basic, Benutzer `opencode`, Passwort = Serverpasswort [getestet: ohne Auth 401, falscher Benutzer 401, richtiger 200].
- Zusätzlich akzeptiert: Query-Parameter `?auth_token=<base64(user:pass)>` und ein **Session-Cookie** `opencode_session_<port>` [Quelle: `middleware/authorization.ts`].
- **Pairing**: `POST /api/pair` (authentifiziert) liefert `{code, expires_in: 300}`. `GET /auth/connect/<code>` ist **unauthentifiziert**, liefert **einmalig** `{"token": "<ablauf>.<hmac>"}` (zweiter Aufruf: 401). Der Token ist ein vom Serverpasswort abgeleiteter HMAC, 30 Tage gültig, und **wird überall als Passwort akzeptiert** (Basic mit Benutzer `opencode`). Eine Passwortänderung widerruft alle Token [getestet, Quelle: `auth.ts`, `pairing.ts`].
- **Unauthentifiziert erreichbar**: die statische Web-UI unter `/` [getestet] und `/auth/connect/<code>`. Alles unter `/api` verlangt Auth.
- Ein Passwort, ein Benutzer. **Keine Benutzerverwaltung, keine Rollen, keine Mehrbenutzer-Trennung** [Quelle]. Jeder Inhaber des Passworts hat die volle Server-Fähigkeit (Shell, Dateien, Provider-Keys nutzen).

### 3.2 CORS und Origin

Erlaubt ohne Konfiguration: `http://localhost:*`, `http://127.0.0.1:*`, `oc://renderer`, `tauri://localhost`, `https://*.opencode.ai`; weitere per `--cors <origin>` (wiederholbar) [Quelle: `cors.ts`]. Same-Host-Origins sind erlaubt. Native Apps senden meist keinen `Origin`-Header (dann erlaubt).

### 3.3 API

- OpenAPI v2: `packages/protocol/openapi.json` (139 Endpunkte, Stand 2026-09-13). Wichtigste: `GET /api/info` (Version, pid, URLs), `GET /api/location`, `GET|POST /api/session`, `POST /api/session/{id}/prompt`, `/command`, `/shell`, `/interrupt`, `/fork`, `/compact`, `GET /api/session/{id}/message`, `GET /api/session/active`, `GET /api/model`, `GET /api/agent`, `GET /api/permission/request`, `POST /api/session/{id}/permission/{requestID}/reply`, `GET /api/event`, `GET /api/experimental/session/{id}/export`, `POST /api/pair` [Quelle].
- Verzeichnis-Modell: Jede Anfrage trägt ein Verzeichnis als Query `location[directory]=<abs. Pfad>` oder Header `x-opencode-directory`; ohne Angabe gilt das `cwd` des Serverprozesses [Quelle: `location.ts`; getestet].
  - Existierender Pfad: wird akzeptiert, **auch außerhalb jedes Repos, auch `/etc`** (dort ein "Projekt" mit eigener ID). Nicht existierender Pfad: `LocationNotFoundError`. Es gibt **keine Workspace-Einschränkung** und keine Allowlist [getestet].
  - Projekt-Config (`opencode.json` im Verzeichnis) wird pro Verzeichnis geladen [getestet: Regel `bash: ask` aus `work2/opencode.json` griff].
- Eine Serverinstanz bedient **beliebig viele Verzeichnisse gleichzeitig**, getrennt nach "Location". Das Betriebssystem-Konto des Prozesses begrenzt den Zugriff, sonst nichts.

### 3.4 Speicherung

- Eine SQLite-Datei `opencode.db` (+ `-wal`, `-shm`) im Datenverzeichnis, dazu `log/`, `snapshot/`, `shell/`, `repos/`, `tool-output/`, `storage/` (Beobachtung im Test und in `debug paths`) [getestet].
  - Pfade (macOS, `opencode debug paths`): Daten `~/.local/share/opencode`, Config `~/.config/opencode`, Cache `~/.cache/opencode`, State `~/.local/state/opencode`. Auf Linux gelten dieselben XDG-Regeln; Verschieben per `XDG_*`, `HOME` oder `OPENCODE_DB` (Doku `cli.mdx`) [getestet für `XDG_*`/`HOME`].
  - Provider-Zugangsdaten liegen in `auth.json` im Datenverzeichnis (nicht gelesen) [ungeprüft: Format, Verschlüsselung; Ablage laut Doku `providers.mdx`].
- Sitzungen sind an den Server und dessen DB gebunden; Export/Import als JSON bringt sie zwischen Instanzen [getestet: `session export --sanitize`].

### 3.5 Web-UI

`opencode serve` (v2) liefert die Web-UI selbst aus, die Dateien sind in die Binärdatei eingebettet (`services/web-ui.ts`, `app-assets`) [getestet, Quelle]. In v1 leitet der Server UI-Anfragen an `https://app.opencode.ai` weiter (`UI_UPSTREAM` in `server/shared/ui.ts`) [Quelle]: dort braucht der Server ausgehenden Zugang zu diesem Host, und die UI-Auslieferung hängt vom Anbieter ab. Der Server sendet beim ersten Aufruf ein Anmeldeformular (Browser: Basic-Prompt nur bei Navigation, `sec-fetch-mode`-Logik in `authorization.ts`).

## 4. Android und andere Clients

- Offizielle Android-App: `ai.opencode.mobilecode` (Herausgeber Anomaly Innovations, Inc., Play Store, zuletzt aktualisiert 2026-11-08). Laut Store-Text braucht sie eine kompatible **OpenCode-V2**-Installation und verbindet per QR-Pairing-Code oder "Verbindungsdetails"; sie zeigt Sessions, streamt Antworten, beantwortet Permission- und Fragen-Prompts, nimmt Bilder und Diktat [Doku: Play-Store-Seite, 2026-11-11]. Der Quellcode der App liegt nicht im Repository `anomalyco/opencode`; **eigene Header, Basic-Eingabe, OIDC, privates CA und Push sind nicht belegt** [ungeprüft].
- Daneben gibt es Drittprojekte, die **nicht** zur offiziellen App gehören, z. B. "OpenCode Mobile" (`app.getopencode`, Play Store), `dzianisv/opencode-mobile`, `theblazehen/P4OC` [Doku: Websuche 2026-11-11]. Ob die Doku-Seite `opencode-mobile-website.pages.dev` zur Play-Store-App `app.getopencode` gehört, ist nicht belegt [ungeprüft].
- Die Doku der Drittapp "OpenCode Mobile" (`opencode-mobile-website.pages.dev/docs/remote-access`) beschreibt: Server-URL plus optional Basic (Benutzer/Passwort), **eigene HTTP-Header pro Verbindung** (z. B. für Gateways wie Cloudflare Access), **privates CA pro `host:port`**, OIDC-Anmeldung nur bei manueller Verbindung, Unterstützung von `opencode pair` per QR/Link, Reverse-Proxy erlaubt, wenn **SSE und WebSocket** (Terminals) durchgereicht werden [Doku, nicht getestet].
- Die offizielle v2-Doku (`opencode.ai/v2/docs/cli/web`) nennt "the OpenCode app" mit QR oder Link-Einfügen [Doku].
- **Hinter einem SSO-Reverse-Proxy (Forward-Auth, Browser-Redirect)**: Eine native App kann keinen Browser-Redirect abwickeln. Es funktionieren nur: (a) Proxy mit eigener Header-Authentifizierung für Maschinenclients (Service-Token-Header; ob die offizielle App Header pro Verbindung senden kann, ist nicht belegt, nur die Drittapp "OpenCode Mobile" dokumentiert es), (b) der Proxy lässt `/api` und `/auth/connect` ohne SSO durch und verlässt sich auf das opencode-Passwort, (c) ein Overlay-Netz (VPN) statt Internet. Alle drei sind **[ungeprüft]** gegen die App; zu klären ist dort auch die Kollision zweier `Authorization`-Header (Proxy-Basic gegen opencode-Basic) [Schlussfolgerung].
- v1-Server: nur Basic mit konfigurierbarem Benutzernamen; Drittapps unterscheiden sich im Umgang mit `OPENCODE_SERVER_USERNAME` [Doku, widersprüchlich].

## 5. Aktivität verfolgen

| Weg | Eigenschaft |
|---|---|
| `GET /api/event` (SSE) | Alle Ereignisse **aller Locations**, **flüchtig**: ein langsamer Konsument überläuft, Ereignisse während Verbindungsabbruch gehen verloren. Erstes Ereignis `server.connected`, Heartbeats als Kommentare. Typen im Test u. a. `session.created`, `session.step.started/ended`, `session.text.delta`, `session.reasoning.delta`, `session.tool.called/success`, `session.execution.started/succeeded`, `shell.created/exited`, `session.inbox.*`, `session.usage.updated` [getestet] |
| `GET /api/experimental/session/{id}/log?after=<seq>&follow=true` (SSE) | **Dauerhaftes Ereignisprotokoll** pro Sitzung mit Sequenznummer; erlaubt Wiederaufnahme ohne Verlust. Als "experimental" markiert [Quelle, nicht getestet] |
| `GET /api/session`, `/api/session/active`, `/api/session/{id}/message`, `/diff`, `/context` | Abfrage des Zustands, ideal zum Pollen und für Abschlussberichte [Quelle; `session` getestet] |
| `opencode run --format json` | JSON-Zeilen mit allen Teilen, direkt in Actions-Logs lesbar [getestet] |
| `opencode session export [--sanitize]`, `GET /api/experimental/session/{id}/export` | Vollständiges Transkript als JSON; `--sanitize` schwärzt sensible Inhalte [getestet] |
| `opencode stats [--json --cost --models --tools]` | Aggregierte Nutzung [Hilfe] |
| Serverlog | `<Datenverzeichnis>/log/opencode.log`; `--print-logs` nur zusammen mit `--standalone` [Hilfe, Dateien beobachtet] |
| OpenTelemetry | `OTEL_EXPORTER_OTLP_ENDPOINT` / `OTEL_EXPORTER_OTLP_HEADERS` / `OTEL_RESOURCE_ATTRIBUTES` werden gelesen [Quelle: `util/src/observability.ts`; Wirkung **[ungeprüft]**] |

## 6. GitHub-Integration (v1-Linie)

- Die Action `anomalyco/opencode/github` führt `opencode github run` im Job aus. Sie installiert über `curl -fsSL https://opencode.ai/install | bash` (also v1), startet mit `spawn("opencode", ["serve", "--hostname=…", "--port=…"])` einen **lokalen Server im Runner**, liest `GET /event` und kommentiert in der Issue/PR; Eingaben `model`, `agent`, `share`, `prompt`, `use_github_token`, `mentions` (Default `/opencode`, `/oc`), `variant` [Quelle: `github/action.yml`, `github/index.ts` im v2-Tag; `github.ts` im v1-Tag].
- Das ist **ein Server pro Job**, kein Anschluss an eine dauerhafte Instanz. `opencode github install` (App-Installation, Workflow, Secrets) gibt es nur in v1 [Doku `github.mdx`].
- `share` ist für öffentliche Repos standardmäßig an (Action-Eingabe), und eine geteilte Session ist öffentlich lesbar (`opncd.ai/s/<id>`) [Quelle: `action.yml`; Doku `share.mdx`]. Auf einer internen Instanz `share` abschalten.

## 7. Modell, Agent, Berechtigungen, Fähigkeiten je Run

- Modell: `--model provider/model#variant`; Standard aus der Konfiguration. Modellliste: `GET /api/model`, `opencode models`. Im Test ohne eigene Provider nur der kostenlose Provider "OpenCode Zen" mit öffentlichem Key (`exo-free`, `iceman-free`) [getestet].
- Agent: `--agent <name>`; Agenten (Prompt, Modell, Tools, `permission`) stehen in `opencode.json` oder `agent/*.md` [Doku `agents.mdx`].
- Berechtigungen: `permission` in der Konfiguration global, je Agent oder je Projektverzeichnis, mit `allow|ask|deny` und Mustern (letzte passende Regel gewinnt); `--auto` hebt `ask` auf `allow`. **`run` hat keinen Schalter für einzelne Regeln.** Variation je Run geht über `--agent` (Agent mit eigener `permission`) oder über die `opencode.json` im Zielverzeichnis [Doku, Quelle; Projekt-Config getestet].
- Config-Quellen: global, `OPENCODE_CONFIG`, Projekt, `OPENCODE_CONFIG_CONTENT` (Inline, **nur beim Start des Serverprozesses**, nicht je Run) [Doku `config.mdx`, `server-process.ts`].
- Fähigkeiten (Skills, MCP, Plugins, Commands): serverseitig konfiguriert (`~/.config/opencode`, Projekt-`.opencode/`); `opencode mcp add`; die API hat eine Gruppe `skill` (`protocol/src/groups/skill.ts`), eine Auswahl je Run bietet `run` nicht, außer über den Agenten [Doku; Auswahl je Run **[ungeprüft]**].

## 8. Lokale Tests (Protokoll)

Aufbau: eigener `HOME` und `XDG_*` in einem Wegwerfverzeichnis, damit weder laufende opencode-Dienste noch
vorhandene Zugangsdaten berührt werden. Server: `opencode serve --hostname 127.0.0.1 --port 4199`
mit Test-Passwort aus `OPENCODE_SERVER_PASSWORD`. Provider: nur der eingebaute kostenlose Zen-Provider
(**kein** konfigurierter Provider, keine Secrets). Ein einziger kurzer Prompt-Typ ("say only the word ok", `touch`-Aufgaben).

| Test | Ergebnis |
|---|---|
| `GET /api/info` ohne Auth / falscher Benutzer / richtig | 401 / 401 / 200 mit `{"version":"2.0.29",…}` |
| `GET /` ohne Auth | 200, HTML der Web-UI |
| `run --server … --format json "say only the word ok"` | JSON-Zeilen `step_start`, `text:"ok"`; Modell `iceman-free`/`exo-free` |
| SSE `/api/event` parallel zu einem Lauf mit `bash`-Tool | Ereignistypen wie in Abschnitt 5 |
| `bash: ask` in Projekt-Config, `run` ohne `--auto` | Ablehnung mit Meldung, Lauf geht weiter, Exit 0, Datei **nicht** angelegt |
| `POST /api/pair` und `GET /auth/connect/<code>` | Token einmalig; zweiter Abruf 401 |
| `GET /api/location` mit `location[directory]=/nonexistent/xyz`, `/etc`, leerem Ordner, Header-Variante | Fehler / akzeptiert / akzeptiert / akzeptiert |
| `session list`, `session export --sanitize`, `run --session <id> --fork` | Funktioniert; Fork erscheint als "(fork #1)" |
| `run --auto` | **nicht ausgeführt** |
| `--mdns`, `--cors`, `acp`, `mini`, `service`/`pair --remote`, `/api/experimental/session/{id}/log` | nicht getestet |

## 9. Ungeprüft / offen

1. Ob die offizielle Android-App Basic-Eingabe, eigene Header, OIDC, privates CA und Push beherrscht [ungeprüft; App ist nicht quelloffen].
2. Verhalten hinter Traefik mit Forward-Auth für SSE, WebSocket und die App [ungeprüft].
3. v1-Linie lokal nicht installiert (Download braucht Freigabe): `run --attach --dir`, `web`, `github run` nur aus dem Quellcode belegt.
4. `--auto` und `opencode github` nicht ausgeführt.
5. Verhalten bei Neustart des Servers mitten im Lauf (Wiederaufnahme laufender Sessions) und Parallelität mehrerer Läufe in einem Verzeichnis [ungeprüft].
6. Dauerhaftes Ereignisprotokoll (`session.log`) nicht getestet.
7. Verschlüsselung von `auth.json`: nicht geprüft.
8. Ob die Doku-Aussage "Pairing braucht den Hintergrunddienst" für einen alleinstehenden `serve` nur die CLI betrifft: `POST /api/pair` funktioniert auf einem alleinstehenden `serve` [getestet]; der CLI-Befehl `pair` nutzt ausschließlich den Dienst [Quelle].
