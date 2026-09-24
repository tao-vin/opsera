# Opsera Relay

Relay is the public rendezvous service for Windows agent connections.

The first implementation keeps the service intentionally small:

- register a Windows device and allocate a machine code
- keep device online status through heartbeat
- verify a fixed access password without storing it in plain text
- expose device lookup for the desktop/controller side

Run locally:

```powershell
go run .\relay
```

Default address:

```text
:18742
```

Use `OPSERA_RELAY_ADDR` to bind another address.

Data is stored in `relay-data/devices.json` by default. Use `OPSERA_RELAY_DATA` to move it on the server.

