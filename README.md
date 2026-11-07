# Warehouse Sync Server

Local sync server for the Warehouse Inventory app. Runs on one business's PC;
the desktop and phone apps on the same network (or through a Cloudflare Tunnel)
share its data. Standard-library Go only.

## Accounts (per server)
A fresh server has no users and works with just the pairing code (as before).
An admin runs first-time **setup** (company + first admin) from the app; after
that, sign-in is required and roles are enforced **on the server**:

- **admin** — full access + user management
- **office** — full operational access incl. cost/finance
- **warehouse** — operational access only; never receives or can alter
  cost/finance data (enforced server-side, not just hidden in the UI)

Credentials live in `sync-data/accounts.json` (PBKDF2-HMAC-SHA256 hashes) and are
never synced to devices.

## Build
GitHub → Actions → **Build Sync Server** → Run workflow. It publishes
`Warehouse Sync Server.exe` as a Release (works with the Build Tool).

## Deploy
Back up the `sync-data` folder, then replace the running
`Warehouse Sync Server.exe` with the new one (keep `sync-data` next to it).

> Note: `scanner_stub.go` is a placeholder for the document-scanner / USB-phone
> code that was not in the uploaded source. Restore the real file before
> deploying so those features are not lost.
