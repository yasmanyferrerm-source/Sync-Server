# Warehouse Sync Server — Ubuntu / Linux

A single static binary. No dependencies to run the server itself. For **printing**
it uses CUPS (the standard Linux print system).

## 1. Put it in place
```bash
sudo mkdir -p /opt/warehouse-sync-server
sudo cp warehouse-sync-server /opt/warehouse-sync-server/
sudo chmod +x /opt/warehouse-sync-server/warehouse-sync-server
```

## 2. Bring your data over (keeps your pairing code + all records)
Copy the whole `sync-data` folder from the Windows machine next to the binary:
```
/opt/warehouse-sync-server/
  warehouse-sync-server
  sync-data/            <-- copied from Windows (Documents\Sync Server\sync-data)
```

## 3. Printing (CUPS)
```bash
sudo apt update && sudo apt install -y cups cups-client
sudo usermod -aG lpadmin $USER      # lets you add printers
# Add your printers in the CUPS web UI: http://localhost:631  (Administration > Add Printer)
# or from GNOME Settings > Printers. HP: also `sudo apt install hplip`.
lpstat -e                           # should list your printers
```
The names shown by `lpstat -e` are what appear in the app's printer picker.

## 4. Run it
Quick test (foreground):
```bash
cd /opt/warehouse-sync-server && ./warehouse-sync-server
```
It prints its address and pairing code, and serves the dashboard at
`http://localhost:8787/status`.

## 5. Run on boot (systemd)
Edit `warehouse-sync-server.service` (set `User=` to your username), then:
```bash
sudo cp warehouse-sync-server.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now warehouse-sync-server
systemctl status warehouse-sync-server      # check it's running
journalctl -u warehouse-sync-server -f        # live logs
```

## 6. Your Cloudflare tunnel
Move `cloudflared` to this machine too, pointing at `http://localhost:8787`, so
`sync.yasmanyfmsoft.com` keeps working and no device needs reconfiguring.
