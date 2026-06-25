# mail-notify

`mail-notify` is a small GNOME-native mail notifier written in Go. It reads mail-capable accounts from GNOME Online Accounts over D-Bus, checks each account's IMAP `INBOX`, and sends desktop notifications through `org.freedesktop.Notifications`.

The first run records a baseline and stays quiet by default. Later checks notify when `UIDNEXT` increases and the mailbox still has unread mail.

## Requirements

- GNOME Online Accounts with mail enabled for the account.
- A notification daemon, normally provided by GNOME Shell.
- Go 1.22 or newer.

For OAuth2 accounts such as Google or Microsoft, GOA supplies an access token. For password-based accounts, GOA must expose the IMAP password through its `PasswordBased` D-Bus interface.

## Build

```sh
go mod tidy
go build -buildvcs=false -trimpath -ldflags "-s -w -buildid=" -o ./bin/mail-notify ./cmd/mail-notify
```

## Run

```sh
./mail-notify -debug
```

Useful flags:

- `-config PATH`: override the config file location.
- `-interval 2m`: polling interval.
- `-once`: perform one check and exit.
- `-notify-existing`: notify about current unread mail during the first baseline run.
- `-runtime-state`: store the state file under `XDG_RUNTIME_DIR`.
- `-state PATH`: override the state file location.

The default config file is:

```text
${XDG_CONFIG_HOME}/mail-notify/config.json
```

or:

```text
~/.config/mail-notify/config.json
```

Example TLS override for `imap.corp.netease.com` when the server presents a certificate for `*.netease.com`:

```json
{
  "notification": {
    "open_command": ["gtk-launch", "org.gnome.Evolution.desktop"]
  },
  "tls_overrides": {
    "imap.mail.example.com": {
      "allowed_cert_names": ["*.example.com", "example.com"]
    }
  }
}
```

`notification.open_command` is optional. When set, mail notifications include a click action and the running notifier starts that command after the notification is clicked. Use an argv-style array instead of a shell string. Other examples:

```json
{ "notification": { "open_command": ["thunderbird"] } }
```

When starting the click command, `mail-notify` copies the current locale variables (`LANG`, `LANGUAGE`, and `LC_*`) from the systemd user manager environment. This keeps desktop clients aligned with `systemctl --user show-environment` even if the notifier process was started with an older locale.

This keeps certificate chain validation enabled, but accepts the configured certificate DNS names for that one IMAP host. If a server also needs a different SNI name, add `server_name`, for example `"server_name": "netease.com"`.

The default state file is:

```text
${XDG_STATE_HOME}/mail-notify/state.json
```

or:

```text
~/.local/state/mail-notify/state.json
```

For per-login ephemeral state, run with:

```sh
./mail-notify -runtime-state
```

That stores state at:

```text
${XDG_RUNTIME_DIR}/mail-notify/state.json
```

If both `-state PATH` and `-runtime-state` are provided, the explicit `-state` path wins.

Each account state stores a `last_seen` timestamp. If an account remains in the state file but no longer appears in GNOME Online Accounts for more than 10 days, its saved state is removed automatically. Older state entries without `last_seen` are first migrated by recording the current time, so they are not deleted immediately.

## systemd user service

Build the binary somewhere stable, then copy and edit the unit:

```sh
mkdir -p ~/.config/systemd/user
cp packaging/mail-notify.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now mail-notify.service
```

If your binary is not at `%h/.local/bin/mail-notify`, edit `ExecStart`.

## Notes

This implementation intentionally polls instead of keeping IMAP `IDLE` connections open. Polling is simpler, survives token refreshes naturally, and is easier to run as a tiny desktop service. `IDLE` can be added later once the basic GOA/IMAP flow is confirmed on the target accounts.
