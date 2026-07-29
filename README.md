# later

[![CI](https://github.com/zawnk/later/actions/workflows/ci.yml/badge.svg)](https://github.com/zawnk/later/actions/workflows/ci.yml)
[![GHCR](https://img.shields.io/badge/ghcr.io-later--server-blue?logo=docker)](https://github.com/zawnk/later/pkgs/container/later-server)
[![License: AGPL v3](https://img.shields.io/badge/license-AGPL--3.0-blue)](LICENSE)

A self-hosted reminder service, tightly coupled with [ntfy](https://ntfy.sh) - set reminders in plain English, get them back as push notifications.

```
later in 25 minutes start the laundry
later in 3d rotate the tires
later next tuesday at 2pm pick up the dry cleaning
later march 3rd sort out the tax paperwork
```

If you're already running ntfy, that's the only other piece you need - just plain JSON files instead of a database and no user accounts needed. Text it or CLI it, get pinged when it's due.

---

## How it works

1. You run `later-server` (Docker) alongside your existing ntfy server.
2. You send it a reminder - either the CLI (`later in 3d buy milk`) or by
   texting to an ntfy topic from your phone.
3. `later` parses the natural language, stores the reminder (plain JSON), 
   and fires an ntfy push notification when it's due.
4. Fired notifications carry one-tap **Snooze 1h** / **Tomorrow** buttons -
   no need to open a terminal just to push something back.

## Features

- **Natural language reminders** - "in 3 days call the plumber", "next
  tuesday at 2pm", "2h30m", whatever's natural to type. No rigid syntax
  to learn.
- **Two ways in** - the CLI (`later in 3d buy milk`) or just texting a
  reminder to your own ntfy topic from your phone, no terminal needed.
- **Not sure it'll understand you? Ask first** - `later test parse tomorrow
  at 2am go to bed` (or text `/test ...` to your ntfy topic) shows
  exactly what would get scheduled - task text and due time - without
  actually creating anything.
- **Texting one in talks back** - reply confirms the exact due time and
  the reminder's id, or explains what went wrong (bad input, a topic your
  token isn't allowed to use, etc.) - you're never left guessing whether
  it actually took.
- **Tag or prioritize from your phone** - add `#tag` / `!high` to the end
  of a message and it carries through (`"call mom tomorrow #family
  !high"`).
- **Real push notifications** - tags, priority, and a tappable link if you
  want one, same as any other ntfy notification. If a reminder's been
  sitting a while before it fires (say the server was briefly down), the
  notification gets a `DELAYED:` prefix, a  warning tag, and a priority bump -
  so it doesn't get lost in the noise.
- **Flexible routing** - each access token can be scoped to its own set
  of ntfy topics. Skip `default_outbound` and reminders with no explicit
  topic fan out to all of them; set it to route unscoped reminders to
  just one instead.
- **Cancel or postpone anytime** - reference a reminder by id, or just
  say `last` in the CLI for the one you created or postponed most
  recently, no need to go copy an id first.
- **One-tap snooze from the notification itself** - fired reminders carry
  "Snooze 1h" / "Tomorrow" buttons (ntfy's own `Actions` feature), each
  with a scoped, short-lived, single-use credential baked in. Opt-in:
  configure `base_url` and it turns on, leave it unset and it doesn't.
- **A proper CLI** - free text, no quotes needed, works with piped input
  too. Sortable/limitable lists, quick access to what's coming up next or
  what just fired, search across pending or fired reminders, `--json`
  output if you want to script it, and simple config via flags,
  environment variables, or a config file.
- **A real HTTP API** - create, list, cancel, and postpone reminders from
  anything that can make an HTTP request. Token-based auth, and the list
  endpoints support the same sort/limit/search options the CLI uses.
- **No database** - just two plain JSON files (`pending.json`,
  `archive.json`) you can read, edit, `jq` through, or back up by hand.
  Nothing to migrate, nothing opaque. Writes are atomic (temp file +
  fsync + rename), so a crash or power loss mid-write can't corrupt
  either file.
- **Docker-ready** - image on GHCR for `amd64` and `arm64`, built on
  `distroless/static` with a real healthcheck baked in (`later
  healthcheck` / `GET /healthz`, no token needed). Timezone data is
  compiled in, so a plain `TZ=Europe/Berlin` env var just works.

## Quick start

You need an ntfy server already running (self-hosted or ntfy.sh) and a
token for it.

**1. Get `later-server` running:**

```yaml
# compose.yml
services:
  later:
    image: ghcr.io/zawnk/later-server:latest
    container_name: later
    restart: unless-stopped
    environment:
      - TZ=Europe/Berlin
    volumes:
      - ./data:/data
    ports:
      - "8080:8080"
    healthcheck:
      test: ["CMD", "/later", "healthcheck"]
      interval: 30s
      timeout: 3s
      start_period: 5s
      retries: 3
```

**2. Configure it** - create `data/config.yaml`:

```yaml
server:
  base_url: https://later.yourdomain.com   # optional: enables Snooze/Tomorrow notification buttons; omit to leave that off

ntfy:
  server: https://ntfy.yourdomain.com
  token: tk_XXXXX                          # your ntfy token

inbound:
  - topic: later-input-me                  # text/send reminder commands here
    outbound: [later-reminders-me]         # reminders from this inbound topic fire here

auth_tokens:
  - token: tk_replace_this_with_a_real_token_at_least_16_chars   # generate your own, e.g. ntfy cli or `openssl rand -hex 16`
    outbound: [later-reminders-me]         # topics this token's reminders are allowed to target
```

`./data` on the host holds both `config.yaml` and the state files
(`pending.json`, `archive.json`) - one directory, everything in it.

See [`data/config.yaml.example`](data/config.yaml.example) for every
option, including optional ones like `default_outbound`.

**3. Run it:** `docker compose up -d`

**4. Try it:**

```
curl -X POST https://later.yourdomain.com/reminders \
  -H "Authorization: Bearer tk_replace_this_with_a_real_token_at_least_16_chars" \
  -H "Content-Type: application/json" \
  -d '{"text": "in 15s test the setup"}'
```

...or just install the CLI and run `later in 15s test the setup`.
Fifteen seconds later, a push notification should land on your phone.

## The CLI

Grab a binary from the [Releases page](https://github.com/zawnk/later/releases?q=later%2F)
(pick one tagged `later/vX.Y.Z`, not `later-server/...` - that one's the
Docker-image release):

```
curl -LO https://github.com/zawnk/later/releases/download/later/v0.1.1/later_v0.1.1_linux_amd64
chmod +x later_v0.1.1_linux_amd64
sudo mv later_v0.1.1_linux_amd64 /usr/local/bin/later
```

(swap `amd64` for `arm64` if that's your machine; a `later_checksums.txt`
ships alongside each release if you want to verify the download)

Only Linux binaries are published right now. On macOS or Windows, build
it yourself instead (needs [Go](https://go.dev) 1.26+):

```
git clone https://github.com/zawnk/later.git
cd later
go build -o later ./cmd/later
```

Then point it at your server - `~/.config/later/config`:

```
later_url = https://later.yourdomain.com
later_token = tk_replace_this_with_a_real_token_at_least_16_chars
```

(`--url`/`--token` flags or `LATER_URL`/`LATER_TOKEN` env vars work too,
and override the config file.)

```
later in 3d buy milk                    # create - no quotes needed
later list                              # pending reminders, soonest first
later archive --limit 10                # last 10 fired reminders
later search milk                       # substring search, pending by default
later next                              # what's coming up next
later cancel last                       # cancel the one you just created
later postpone last 1h                  # reschedule a fired reminder in an hour
later test parse tomorrow at 2am sleep  # preview parsing, creates nothing
```

Add `--json` to any read command for machine-readable output.

## Natural language examples

Not an exhaustive list - just enough to show the range. All of these are
also available by text message straight to your ntfy inbound topic.

**Durations:**
```
later in 3d rotate the tires
later in 2h30m check on the laundry
later in 1w2d water the tomatoes
later in 3 days call the plumber for a quote
later in 45 minutes take the bread out of the oven
```

**Casual dates/times:**
```
later tomorrow evening call grandma
later tonight take out the recycling
later this afternoon reply to the neighbor about the fence
```

**Weekdays:**
```
later next tuesday renew the library books
later this friday order more coffee beans
```

**Exact calendar dates:**
```
later march 3rd sort out the tax paperwork
later 1st of september start the fantasy football draft prep
later 28/07/2026 book the campsite for the summer trip
```

**Specific times:**
```
later at 5pm start marinating the chicken for dinner
later at 2:30pm join the team standup
```

**Combined:**
```
later next tuesday at 2pm pick up the dry cleaning
```

> `later at 5pm ...`-style examples resolve relative to *now* - if it's
> already past 5pm today, that's a past-due rejection, not a bug. Add
> `tomorrow` (`later tomorrow at 5pm ...`) if you're trying this late in
> the day, or just lean on the duration-based examples above, which never
> go stale.

**Tag or prioritize inline (texted reminders only), trailing, any combination:**
```
call mom tomorrow #family !high
water the office plants next monday #chores
```

## HTTP API

Token-based auth (`Authorization: Bearer <token>`), JSON in and out.

| Method | Path | Notes |
|---|---|---|
| `POST` | `/reminders` | Create. Body: `{"text": "...", "outbound_topics": [...], "tags": [...], "priority": "...", "click": "..."}` - only `text` is required. |
| `GET` | `/reminders` | List pending. `?sort=due\|create`, `?q=<substring>`. |
| `GET` | `/reminders/{id}` | Fetch one, pending or archived. |
| `GET` | `/reminders/archive` | List fired. `?limit=N`, `?q=<substring>`. |
| `GET` | `/reminders/next` | The soonest-due pending reminder. |
| `GET` | `/reminders/last` | The most recently fired reminder. |
| `DELETE` | `/reminders/{id}` | Cancel a pending reminder. |
| `POST` | `/reminders/{id}/postpone?duration=1h` | `duration` is a query param - compact or natural language. |
| `POST` | `/test/parse` | Preview parsing. Body: `{"text": "..."}`. Creates nothing. |
| `GET` | `/healthz` | No token needed. |

Errors come back as `{"error": "..."}` with a non-2xx status.

## Known limitations

- If you rotate or purge `archive.json` (neither should be necessary in
  normal use), be aware some functionality references archived reminder
  IDs - postpone, for instance.
- The notification action buttons (Snooze/Tomorrow) are tapped from your
  *phone*, calling `base_url` directly - so a private LAN `base_url` only
  works when the device tapping the button can actually reach it (same
  network, reverse proxy or a VPN back to it).
- A bare `base_url` with no scheme (e.g. `192.168.1.53:8080`) is assumed
  to be `http://`, not `https://` - fine for most home networks; put a
  scheme on it yourself once you have TLS in front of it.

## Why not X?

- **A web UI** - considered out of scope for this project but there's always the HTTP API.
- **A database** - the JSON files being `jq`-able is a feature, not a gap.
- **Multi-user / accounts** - this is a homelab tool for one person (or
  one household sharing tokens), not a SaaS.
- **Supporting notification backends other than ntfy** - the tight ntfy
  coupling is the point. `later` leans on ntfy for delivery, priorities,
  tags, click actions, and the action-button mechanism instead of
  reimplementing all of it.

## License

[AGPL-3.0](LICENSE)
