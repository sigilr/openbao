<!--
Copyright (c) AppsCode Inc.
SPDX-License-Identifier: MPL-2.0
-->

# Remote Database Plugin — Design

A hub-and-spoke deployment of OpenBao's database secrets engine. One OpenBao
instance (**the hub**) brokers credential operations over mTLS gRPC to one or
more `bao agent run` daemons (**the spokes**) that run in-process database
plugins against locally-reachable databases.

Operators install one binary — `bao` — and run different subcommands on the
hub and the spokes.

```
                                       ┌──────────────────────────────────┐
                                       │   spoke cluster A                │
                                       │                                  │
                                       │   bao agent run                  │
                ┌─────────── mTLS ─────►│   ├─ postgresql-database-plugin  │
                │  gRPC                 │   │   (in-process, cached)       │
                │  (proxy port 50053)   │   └─→ postgres@127.0.0.1:5432    │
                │                       └──────────────────────────────────┘
┌───────────────┴────────────────┐
│   hub OpenBao                  │      ┌──────────────────────────────────┐
│                                │      │   spoke cluster B                │
│   agent/ logical backend       │      │                                  │
│   ├─ bootstrap tokens          │      │   bao agent run                  │
│   ├─ spoke-CA                  │◄────►│   ├─ mysql-database-plugin       │
│   └─ hub TLS identity          │mTLS  │   │   (in-process, cached)       │
│                                │      │   └─→ mysql@127.0.0.1:3306       │
│   remote-{postgres,mysql,...}- │      └──────────────────────────────────┘
│   proxy database plugins       │
│                                │      ┌──────────────────────────────────┐
│   bao agent init / join / list │      │   operator workstation           │
│   bao agent ca status / rotate │      │   bao agent init (once on hub)   │
│   bao agent token create/...   │      │   bao agent join (once per spoke)│
└────────────────────────────────┘      └──────────────────────────────────┘
```

---

## File map

| Path | Role |
| --- | --- |
| `proxy.go` | Hub-side proxy plugin (`PluginProxy`) + proxy gRPC server. One server, many connected spokes. |
| `runner/runner.go` | Spoke-side plugin dispatcher with the per-instance cache. |
| `bootstrap/token.go` | Bootstrap token format + detached JWS-HS256 sign/verify. |
| `bootstrap/pubkeypin.go` | SPKI SHA-256 hash + verification. |
| `bootstrap/ca.go` | Spoke-CA generation, hub TLS cert issuance, CSR signing. |
| `bootstrap/state.go` | Process-wide singleton holding the CA + hub cert; shared between the agent backend and the proxy server. |
| `proto/agent.proto` | gRPC contract. One bidi stream per spoke. |
| `../../builtin/logical/agent/{backend,paths}.go` | The `agent/` logical backend. Operators interact with it via `bao agent ...`. |
| `../../command/agent_{init,join,list,run,ca,token}.go` | The `bao agent ...` CLI subcommands. |

---

## Trust bootstrap

The bootstrap is a port of kubeadm's discovery flow. Four primitives:

1. **Bootstrap token** — `<6-char-id>.<16-char-secret>`, stored in seal-wrapped
   logical storage. Secret half is the HMAC key, id is the lookup key.
2. **Cluster-info bundle** — `{ca_cert_pem, hub_endpoint}` returned by the hub
   over the standard OpenBao API.
3. **JWS-HS256 over cluster-info** — the hub signs the bundle with the token's
   secret half. Only the real hub can produce a matching signature.
4. **SPKI pin** — `sha256(DER(SubjectPublicKeyInfo))` of the spoke-CA, printed
   by `bao agent init` and verified by `bao agent join`.

After the bootstrap, the spoke holds an mTLS client cert signed by the
spoke-CA; the hub holds nothing token-shaped — just the CA's public cert in
its `ClientCAs` pool.

### Init (hub operator)

`bao agent init -hub-endpoint=host:port` (`command/agent_init.go`):

1. Mount `agent/` if not already mounted.
2. `agent/ca/init` — generate a fresh self-signed ECDSA P-256 spoke-CA and the
   hub TLS cert (signed by it). Persist both, plus the configured endpoint,
   under `ca/bundle` in storage.
3. Push the identity into `bootstrap.Global()` and start the proxy gRPC
   listener on the endpoint's port via `remotedb.StartProxyServer`.
4. `agent/bootstrap-tokens` — generate a token, persist it under
   `tokens/<id>`. Operator-supplied options: TTL, `allowed_spoke_name`,
   description, usages.
5. Print the join command, including the SPKI pin of the spoke-CA.

### Join (spoke operator)

`bao agent join -token=... -hub-cert-hash=...` (`command/agent_join.go`):

1. Fetch `agent/cluster-info?token_id=<id>` (unauthenticated API path). TLS to
   the OpenBao API is verified via the operator's standard flags
   (`-ca-cert`, `-tls-skip-verify`).
2. **Verify the JWS** against the token's secret half. If this fails, abort —
   we are not talking to the hub that issued the token.
3. **Verify the SPKI pin** against the CA cert in the bundle. If this fails,
   abort.
4. Generate a P-256 keypair, build a CSR with `CN=<spoke-name>`.
5. `agent/sign-csr` with `(token, spoke_name, csr_pem)`. The hub re-validates
   the token (id+secret+usage+`allowed_spoke_name`), signs the CSR via the
   spoke-CA, returns `{cert_pem, ca_cert_pem}`.
6. Write `cert.pem`, `key.pem`, `ca.pem` to `-credentials-dir`.

### Run (spoke daemon, long-running)

`bao agent run -server=<hub:50053> -credentials-dir=...` (`command/agent_run.go`):

1. Load credentials. Spoke identity = `cert.Leaf.Subject.CommonName`.
2. Dial the hub's gRPC port with mTLS + gRPC HTTP/2 keepalive.
3. Open the `AgentService.Connect` bidi stream; send a registration frame.
4. Goroutine A: tick a heartbeat (`IsHeartbeat=true`) every
   `-heartbeat-interval`.
5. Goroutine B (`for stream.Recv()`): dispatch every inbound request frame on
   a bounded worker pool to `runner.ExecuteRequest`. Echo `RequestId` back on
   the response.

The hub-side `proxyServer.Connect` (`proxy.go`) extracts the spoke identity
from the verified peer cert CN — the `client_name` wire field is informational
only and not trusted.

---

## Wire protocol

One service, one RPC:

```protobuf
service AgentService { rpc Connect(stream AgentMessage) returns (stream AgentMessage); }

message AgentMessage {
  string client_name  = 1;  // informational; hub trusts peer-cert CN instead
  string command      = 2;  // JSON request payload (hub -> spoke)
  string output       = 3;  // JSON response payload (spoke -> hub)
  bool   is_response  = 4;
  string target_name  = 5;
  bool   is_heartbeat = 6;  // spoke -> hub, idle liveness
  string request_id   = 7;  // pairs a response with its request
  string error        = 8;  // structured error on the response
}
```

Every hub-issued request carries a fresh `request_id` (12-byte hex). The hub
keeps `inflight map[reqID]chan pendingResponse` per spoke; the dispatch
goroutine inside `proxyServer.Connect` looks up the channel by `request_id`
when a response arrives. This is what lets many `RunCommand` callers be in
flight against one spoke concurrently — the old single-`respCh` + per-spoke
mutex design serialized them.

Two complementary liveness layers:

- **gRPC HTTP/2 keepalive** (`grpc.KeepaliveParams` on both sides) catches
  TCP-level death within ~40s.
- **Application heartbeat** (`is_heartbeat=true` from the spoke every 15s by
  default) catches "TCP alive, spoke loop wedged" within
  `SpokeStaleAfter = 45s`. Every received frame — heartbeat, response, or
  registration — refreshes `lastSeen`, so responses double as heartbeats
  during active traffic.

`bao agent list` reads both signals via `ListConnectedSpokes()` (proxy.go):

```
Listener: :50153
Connected: 1 total, 1 healthy (stale after 45s)

NAME       LAST SEEN  UPTIME  HEALTH
demo       0s ago     11s     OK
```

---

## Request lifecycle

`PluginProxy` is what OpenBao instantiates per database mount. Its
responsibilities are minimal: tag every outbound request with a stable
`instance_id`, marshal args to JSON, hand them to the proxy server.

### Initialize (first call per mount)

1. OpenBao calls `PluginProxy.Initialize(req)`.
2. Mint or read `plugin_instance_id` from `req.Config`. First time it is a
   fresh 12-byte hex; on plugin reload or OpenBao restart the previously
   persisted id is reused.
3. Hub sends `{method: "Initialize", instance_id, plugin_name, config,
   verify_connection}` to the spoke via `RunCommand`.
4. Spoke's `runner.handleInitialize` constructs the actual plugin
   (`postgresql-database-plugin`, etc.), Initializes it, stores it in the
   cache:

   ```go
   r.plugins[instanceID] = &pluginEntry{db: plugin, ...}
   ```

5. Hub appends `spoke_name` and `plugin_instance_id` to the response config,
   which OpenBao persists on the mount. The id survives restarts.

### NewUser / UpdateUser / DeleteUser

1. Hub sends `{method, instance_id, ...}`.
2. Spoke's `runner.withPlugin` looks up the instance:
   - **Cache hit**: dispatch the method on the cached plugin. No
     re-Initialize, no DB connection churn.
   - **Cache miss** (spoke restarted, hub still holds the id): lazy-init from
     the `config` the hub embedded in the request, cache, then dispatch.
3. Spoke marshals the response. Hub's `RunCommand` waiter unblocks on the
   matching `request_id`.

### Close

`PluginProxy.Close()` sends `{method: "Close", instance_id}`. Spoke's
`runner.handleClose` removes the entry and calls `db.Close()` to release the
DB connection. Idempotent: closing an unknown id is a no-op.

The earlier subprocess-per-request design rebuilt the plugin (and the DB
connection) on every call. That broke any plugin state that has to live
between calls — most notably the postgres root-credential rotation flow,
where the new password the plugin produces is silently dropped when the next
call re-Initializes from the stale config.

---

## Operator workflow

```
operator on hub                       operator on each spoke
---------------                       ----------------------
$ bao agent init \
    -hub-endpoint=hub:50053 \
    -hub-dns-sans=hub

prints:
  bao agent join \
      -hub-addr=hub:50053 \
      -hub-cert-hash=sha256:abcd... \
      -token=a6b2fa.fd41cda24a...
                                      $ bao agent join \
                                          -address=https://hub:8200 \
                                          -hub-addr=hub:50053 \
                                          -hub-cert-hash=sha256:abcd... \
                                          -token=a6b2fa.fd41cda24a... \
                                          -spoke-name=spoke-1

                                      prints:
                                        bao agent run \
                                            -server=hub:50053 \
                                            -credentials-dir=/etc/openbao-spoke

                                      $ bao agent run ...      (as a daemon)
$ bao agent list
$ bao secrets enable database
$ bao write database/config/my-db \
    plugin_name=remote-postgres-proxy\
    spoke_name=spoke-1 ...
```

Day-2 operations:

- `bao agent token create` — issue a fresh token (24h TTL by default).
- `bao agent ca status` — show CA + hub cert subjects, expiry, SANs, listener
  port.
- `bao agent ca rotate` — re-issue the hub TLS cert from the existing CA.
  Transparent to running spokes (they still trust the CA).
- `bao agent ca rotate -full -yes` — regenerate the spoke-CA. **Destructive**:
  every issued spoke cert becomes invalid on its next handshake. Operators
  must re-join every spoke.

---

## Failure modes

| Failure | What happens | Recovery |
| --- | --- | --- |
| Spoke process killed | Hub's `Connect` returns; `failAll` releases parked waiters with an error; the spoke disappears from `bao agent list` | `bao agent run` restarts; reconnects with the same cert |
| Spoke loop wedged (TCP alive) | gRPC PINGs still respond, but app heartbeats stop; after 45s the spoke shows `STALE` in `bao agent list` | Same — restart `bao agent run` |
| TCP/network dropped | gRPC keepalive notices within ~40s and tears the connection down on both sides | The spoke daemon reconnects on its retry policy |
| Hub OpenBao restarts | Agent backend hydrates from storage; proxy listener restarts on the same port; existing spoke connections die and the spokes reconnect | Automatic |
| Spoke restarts but hub keeps the old `plugin_instance_id` | First NewUser hits cache miss; runner re-Initializes from the request's config | Automatic — self-healing |
| Bootstrap token expires | `agent/cluster-info` and `agent/sign-csr` return "token unknown or expired" | `bao agent token create` on the hub |
| Spoke cert about to expire | `bao agent run` checks expiry on a ticker (`-renew-check-every`, default 1h) and renews once the cert is past `-renew-threshold` (default 0.5, i.e. half-life). Operators can also force `bao agent renew` directly. | Automatic. Live gRPC connections stay on the old cert until they reconnect, which is why we renew well before expiry. |

---

## Security boundary summary

| Surface | Authenticated by |
| --- | --- |
| `agent/cluster-info`, `agent/sign-csr` | Bootstrap token + JWS-HS256 signature over the response payload. TLS to the OpenBao API is verified via the standard `-ca-cert`/`-tls-skip-verify` flags. |
| Hub proxy gRPC listener | mTLS. Hub presents a cert signed by the spoke-CA; client must present a cert signed by the same CA. Spoke identity comes from the verified peer cert CN. |
| Hub bao API | Standard OpenBao authentication. `agent/cluster-info` and `agent/sign-csr` are in `PathsSpecial.Unauthenticated` because they self-authenticate via the bootstrap token. |
| Spoke-CA + hub key material | Persisted under `ca/bundle` with `SealWrapStorage`. |
| Bootstrap tokens | Persisted under `tokens/<id>` with `SealWrapStorage`. Secret half is stored in cleartext (the JWS HMAC needs it) — seal-wrap mitigates. |
