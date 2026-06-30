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
| `TEST.md` | Step-by-step manual test plan (smoke, token security, CSR validation, renewal, CA rotation, failure modes, concurrency). |

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
   Verify the leaf chains to the bundled `ca.pem` before dialing — a
   half-rotated credentials directory (fresh `cert.pem`, stale `ca.pem`
   from before a CA rotation, or vice versa), an expired cert, or a
   cert with the wrong EKU fails here with "spoke cert in <dir> failed
   verification: <specific x509 cause>" instead of an opaque TLS
   handshake error at first gRPC dial. The wrapped error names the
   exact x509 cause (unknown authority, expired, KU mismatch, etc.)
   so operators don't have to guess between "wrong ca.pem" and
   "cert.pem expired, run join again". Mirrors the hub-side chain-check
   in `bootstrap.SetIdentity`.
2. Dial the hub's gRPC port with mTLS + gRPC HTTP/2 keepalive. Both
   sides pin a **TLS 1.3 floor**.
3. Open the `AgentService.Connect` bidi stream; send a registration frame.
4. Goroutine A: tick a heartbeat (`IsHeartbeat=true`) every
   `-heartbeat-interval`.
5. Goroutine B: tick cert renewal every `-renew-check-every`. When the cert
   is past `-renew-threshold` of its lifetime, call `AgentService.RenewCert`
   over the existing mTLS connection; atomically swap the new cert + key in
   place under `-credentials-dir`.
6. Goroutine C: idle-evict cached plugin instances (`runner.DefaultIdleTTL`,
   24h). Skips entries with an in-flight handler refcount > 0.
7. Goroutine D (`for stream.Recv()`): dispatch every inbound request frame on
   a bounded worker pool to `runner.ExecuteRequest`. Echo `RequestId` back on
   the response.

The hub-side `proxyServer.Connect` (`proxy.go`) extracts the spoke identity
from the verified peer cert CN — the `client_name` wire field is informational
only and not trusted.

---

## Wire protocol

One service, two RPCs:

```protobuf
service AgentService {
  rpc Connect(stream AgentMessage) returns (stream AgentMessage);
  rpc RenewCert(RenewCertRequest) returns (RenewCertResponse);
}

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

message RenewCertRequest  { bytes csr_pem = 1; int64 ttl_seconds = 2; }
message RenewCertResponse { bytes cert_pem = 1; bytes ca_cert_pem = 2; }
```

`RenewCert` is authenticated by the caller's existing mTLS client cert — the
gRPC handshake proves the spoke holds a valid cert signed by the spoke-CA.
The hub enforces that the CSR's CN matches the verified peer-cert CN so
renewal cannot rebind to a different identity. The CA caps the signed cert
at `RenewCertMaxTTL` (90d); a `ttl_seconds == 0` request gets the default
`RenewCertDefaultTTL` (30d), matching what `bao agent join` initially
issues. The initial-issue path (`agent/sign-csr`) uses the same 90d ceiling
via the `maxSpokeCertExpiry` constant in the agent backend. After signing, the
hub re-records the renewed `NotAfter` on the live `spokeConnection` so
`agent/spokes` reports the fresh expiry without waiting for a reconnect (see
"Per-spoke client-cert expiry").

CSR validation on both `sign-csr` and `RenewCert` is strict: only ECDSA or
RSA ≥ 2048 are accepted; SANs (DNS / IP / URI / email) and `ExtraExtensions`
cause immediate rejection; the requested CN is denylisted against
`openbao-hub` and `openbao-spoke-ca` so a malicious spoke cannot ask for a
cert that aliases the hub or the CA itself. Both entry points decode the
PEM envelope via the shared `bootstrap.DecodeCSRPEM` helper, so trailing
data and block-type substitution are rejected the same way regardless of
which path the CSR arrives on.

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

NAME       LAST SEEN  UPTIME  CERT EXP  HEALTH
demo       0s ago     11s     29d       OK
```

### Per-spoke client-cert expiry

The hub terminates each spoke's mTLS stream, so it already holds the verified
client (leaf) certificate. `Connect` records `leaf.NotAfter` on the
`spokeConnection` (`certNotAfter`, guarded by the same mutex as `lastSeen`), and
`ListConnectedSpokes()` surfaces it as `SpokeStatus.CertNotAfter`. The backend
`agent/spokes` path exposes it as `cert_not_after` (Unix seconds, `0` when the
hub never captured a verified peer cert), alongside `ca_not_after` /
`hub_cert_not_after` from `agent/ca/info`. The `CERT EXP` column above is this
value rendered as a relative duration.

Because cert renewal happens **in place over the live stream** — the spoke does
not reconnect (see the renewal note below) — a value captured only at connect
time would go stale after a renewal. So `RenewCert` re-records the connection's
`certNotAfter` from the cert it just signed, under the same lock. The downstream
KubeVault hub operator reads `cert_not_after` per spoke to populate
`VaultAgent.status.certExpiry` for the bootstrap (`bao agent join`) flow.

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

`PluginProxy.Close()` sends `{method: "Close", instance_id}`. The spoke's
`runner.handleClose` drops the cache slot's reference; the actual
`db.Close()` runs once the last in-flight handler releases its own
reference. This is what makes Close safe to call while a
NewUser/UpdateUser/DeleteUser is mid-flight on the same `instance_id` — the
old design (close-on-remove) would have left the in-flight handler running
against an already-closed DB connection. Close is also idempotent: the
hub-side `PluginProxy.Close` clears `p.instanceID` after the first call so a
second invocation short-circuits without another network round-trip, and
closing an unknown id is a no-op on the spoke.

The same refcount discipline makes re-`Initialize` for an already-cached id
safe: `installOrReplace` swaps in the new entry under the lock and drops
the slot ref on the displaced one, but its DB connection stays open until
the last handler that took a ref before the swap releases. The spoke
runner also runs a background idle evictor (`runner.evictIdle`, default
`DefaultIdleTTL = 24h`) that catches the case where Close never arrived —
process crash mid-teardown, mount deleted while the spoke was offline, hub
forgot the `instance_id` after a restart. The evictor only drops the slot
ref on entries whose total refcount is exactly 1 (the slot itself, no
in-flight handler), so a long-running call cannot have its DB connection
closed underneath it.

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
    plugin_name=remote-postgres-plugin\
    spoke_name=spoke-1 ...
```

Day-2 operations:

- `bao agent token create` — issue a fresh token (24h TTL by default).
- `bao agent ca status` — show CA + hub cert subjects, expiry, SANs, listener
  port.
- `bao agent ca rotate` — re-issue the hub TLS cert from the existing CA.
  Transparent to running spokes (they still trust the CA).
- `bao write agent/ca/update-endpoint hub_endpoint=... hub_dns_sans=...` —
  refresh what cluster-info advertises plus the SANs on the hub TLS cert,
  without touching the CA. Useful when the load balancer DNS or the
  advertised endpoint changes. The bound port cannot change here; that
  requires a process restart with the new endpoint already persisted.

  Note: this updates what *future* `bao agent join` calls discover via
  cluster-info. Already-running spoke daemons keep dialing whatever
  `-server` they were configured with at launch; if the hostname/IP they
  point at moves, you have to update their daemon configuration out of
  band. The SAN refresh ensures their TLS handshake against the new
  hostname still validates (the hub cert chains to the same CA).

- `bao agent ca rotate -full -yes` — regenerate the spoke-CA. **Destructive**:
  every issued spoke cert becomes invalid on its next handshake. Operators
  must re-join every spoke and redistribute `ca.pem` out of band — there is
  no in-band channel that survives a full rotation.

---

## Failure modes

| Failure | What happens | Recovery |
| --- | --- | --- |
| Spoke daemon receives SIGINT/SIGTERM | `bao agent run` cancels the stream context, waits for in-flight workers, cancels the heartbeat/renewal goroutines, drains the send channel, and calls `runner.Shutdown` to close every cached plugin's DB connection cleanly. Exit code 0. | None — graceful exit. Restart `bao agent run` to reconnect. |
| Spoke process killed | Hub's `Connect` returns; `failAll` releases parked waiters with an error; the spoke disappears from `bao agent list` | `bao agent run` restarts; reconnects with the same cert |
| Spoke loop wedged (TCP alive) | gRPC PINGs still respond, but app heartbeats stop; after 45s the spoke shows `STALE` in `bao agent list` | Same — restart `bao agent run` |
| TCP/network dropped | gRPC keepalive notices within ~40s and tears the connection down on both sides | The spoke daemon reconnects on its retry policy |
| Hub OpenBao restarts | Agent backend hydrates from storage; proxy listener restarts on the same port; existing spoke connections die and the spokes reconnect | Automatic |
| Spoke restarts but hub keeps the old `plugin_instance_id` | First NewUser hits cache miss; runner re-Initializes from the request's config | Automatic — self-healing |
| Bootstrap token expires | `agent/cluster-info` and `agent/sign-csr` return "token unknown or expired" | `bao agent token create` on the hub |
| Spoke cert about to expire | `bao agent run` checks expiry on a ticker (`-renew-check-every`, default 1h) and renews once the cert is past `-renew-threshold` (default 0.5, i.e. half-life). Operators can also force `bao agent renew` directly. | Automatic. Live gRPC connections stay on the old cert until they reconnect, which is why we renew well before expiry. |
| Two daemons sharing one `-credentials-dir` | Same peer-cert CN, so the hub's reconnect logic kicks whichever connected first off the Connect stream every time the other connects. `bao agent list` shows the spoke flipping in and out and neither daemon makes useful progress. | `bao agent join` refuses to overwrite a non-empty credentials dir without `-force`; operators get a clear error pointing at the actual misconfiguration before they hit it at runtime. |
| Spoke credentials inconsistent (cert.pem from one CA + ca.pem from another, expired cert, KU mismatch, e.g. a half-completed re-join or a partial restore from backup) | `bao agent run` and `bao agent renew` `loadSpokeTLS` runs `leaf.Verify` against the bundled CA pool at startup and returns `spoke cert in <dir> failed verification: <x509 cause>` before gRPC ever dials — the wrapped cause names the specific failure (unknown authority, expired, etc.). | Re-run `bao agent join` with `-force` to refresh the directory atomically; ca.pem and cert.pem come back paired. |

---

## Security boundary summary

| Surface | Authenticated by |
| --- | --- |
| `agent/cluster-info`, `agent/sign-csr` | Bootstrap token + JWS-HS256 signature over the response payload. TLS to the OpenBao API is verified via the standard `-ca-cert`/`-tls-skip-verify` flags. Token failures (malformed format, unknown id, expired, wrong secret, missing `signing` usage, `allowed_spoke_name` mismatch) all return the same generic `"token unknown or expired"` so a holder of one valid token cannot probe other token ids for their policy metadata; real reasons are logged server-side at WARN. `handleSignCSR` additionally evaluates every per-token check (secret HMAC, expiry, usage, allowed_spoke_name) against a placeholder when the id is unknown, so "unknown id" pays the same per-field cost as "known id, wrong secret" — closing the timing oracle between those two branches. Storage-read latency may still differ slightly between hit and miss; pair with the `sys/quotas/rate-limit` policies under "Hardening recommendations" to make brute-force timing impractical. |
| Hub proxy gRPC listener | mTLS, **TLS 1.3 floor on both sides** (`bao agent run` pins TLS 1.3 in its client config too). Hub presents a cert signed by the spoke-CA; client must present a cert signed by the same CA. Spoke identity comes from the verified peer cert CN. The hub cert is verified to chain to the configured CA on every `SetIdentity` call so a corrupted (cert, CA) pair fails up front instead of at first handshake. `loadSpokeTLS` does the symmetric check on the spoke side: the local cert is verified to chain to the bundled `ca.pem` before gRPC dials, so a half-rotated credentials directory fails at startup rather than at handshake time. |
| Hub bao API | Standard OpenBao authentication. `agent/cluster-info` and `agent/sign-csr` are in `PathsSpecial.Unauthenticated` because they self-authenticate via the bootstrap token. |
| Spoke-CA + hub key material | Persisted under `ca/bundle` with `SealWrapStorage`. |
| Bootstrap tokens | Persisted under `tokens/<id>` with `SealWrapStorage`. Secret half is stored in cleartext (the JWS HMAC needs it) — seal-wrap mitigates. |
| SPKI pin verification | `subtle.ConstantTimeCompare` over the lowercase hex hash. The error returned to callers is generic; computed and expected hashes are logged locally so an attacker serving a malicious cluster-info bundle cannot grind a colliding pin via response timing. |

### Hardening recommendations

These are not enforced by the code; they are the operator-side knobs that
keep the unauthenticated discovery surface tight.

- **Rate-limit `agent/cluster-info` and `agent/sign-csr`.** Both are in
  `PathsSpecial.Unauthenticated`. The token id space is small (~16M values),
  and while a valid id alone leaks nothing usable (the JWS still needs the
  64-bit secret), an unthrottled probe load can still be loud. Apply a
  `sys/quotas/rate-limit` policy:

  ```bash
  bao write sys/quotas/rate-limit/agent-cluster-info \
      path=agent/cluster-info rate=10 interval=1m
  bao write sys/quotas/rate-limit/agent-sign-csr \
      path=agent/sign-csr rate=10 interval=1m
  ```

- **Wrap or audit-scrub the `bao agent token create` response.** The token
  appears in cleartext in the API response (operators need to see it once).
  Enable response wrapping or scrub the response from audit devices that
  forward elsewhere.

- **Restrict `agent/bootstrap-tokens` to a small operator group** via a
  policy attached to the token used to call `bao agent token create`. The
  default mount has no ACL above OpenBao root.
