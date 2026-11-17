# Warehouse Sync Server

Local sync server for the Warehouse Inventory app. Runs on one business's PC;
the desktop and phone apps on the same network (or through a Cloudflare Tunnel)
share its data. Standard-library Go only.

## Accounts and companies
A fresh server has no users and works with just the pairing code (as before).
An admin runs first-time **setup** (company + first admin) from the app; after
that, sign-in is required and roles are enforced **on the server**:

- **admin**: full access, user management, company settings (one per company;
  use **Make administrator** to hand the role to someone else)
- **office**: full operational access including cost and finance
- **warehouse**: operational access only; never receives or can change cost and
  finance data (items' costs, expenses, vendor and carrier invoices), enforced
  on the server, not just hidden in the app

The full permission list is in `permissions.go` and is sent to the app at sign-in.

**Companies.** The server's own data is the default company. With
`"hosted": true` in `sync-config.json` the server can also hold other companies
(each in `sync-data/companies/<id>/`), and signed-in users of those companies
sync without the pairing code. Leave `hosted` off (the default) unless this
server is meant to serve the store versions of the app.

**Sign-in.** Newer apps get a 15-minute access token and a 30-day refresh token
that changes on every use (a reused refresh token signs that device out). Older
apps get a 30-day token. Sessions are saved hashed in `sync-data/sessions.json`,
so restarting the server no longer signs everyone out. After 10 wrong passwords
for one account (or 30 from one address) sign-in pauses for 15 minutes.

**Offline.** At sign-in the app also gets a 7-day offline pass signed with the
server's key (`sync-data/offline-key.pem`). Keep that file in your backups and
never share it. The server still checks every change against the user's current
role when the device syncs again.

**Locked out.** Admins get a one-time **recovery code** at setup (and can make a
new one in My Account); "Forgot password?" on the sign-in screen uses it. The
server owner can also reset anyone from the PC that runs the server:

- the dashboard's **Owner tools** card, or
- `warehouse-sync-server --reset-password someone@example.com`

Either gives a temporary password the user must change. Owner tools never work
from another device or through the tunnel, and `status.json` no longer shows the
pairing code, users or devices to requests that arrive through the tunnel.

Passwords are PBKDF2-HMAC-SHA256 (600,000 rounds; older hashes are upgraded at
the next sign-in) in `sync-data/accounts.json`, never synced to devices. Account
events are logged to `sync-data/audit.log`. An older `accounts.json` is upgraded
in place on first start and the original is kept as `accounts.v1.json`.

## Build
GitHub → Actions → **Build Sync Server** → Run workflow. It publishes
`Warehouse Sync Server.exe` as a Release (works with the Build Tool).

## Test
`go test ./...`

## Deploy
Back up the `sync-data` folder, then replace the running
`Warehouse Sync Server.exe` with the new one (keep `sync-data` next to it).

> Note: `scanner_stub.go` is a placeholder for the document-scanner / USB-phone
> code that was not in the uploaded source. Restore the real file before
> deploying so those features are not lost.
