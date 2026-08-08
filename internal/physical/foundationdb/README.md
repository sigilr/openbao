# FoundationDB storage backend

Unlike every other storage backend in this tree, FoundationDB requires CGo
and the native `libfdb_c` client library, so it is not compiled into
standard `bao` builds. Attempts to use `foundationdb` storage in a build
produced without following the steps below fail at runtime with a
descriptive error message (`FoundationDB backend not available in this
OpenBao build`), rather than at compile time.

## Installing the native client library

Download and install the `foundationdb-clients` package matching your
target FoundationDB cluster's version from the
[apple/foundationdb releases page](https://github.com/apple/foundationdb/releases)
(`.deb` and `.rpm` packages are published for Linux; there is currently no
official macOS package for recent releases). This provides both the
`fdb_c.h` header and `libfdb_c` shared library that the Go bindings link
against.

The minimum FoundationDB API version supported by this backend is 520;
the requested `api_version` in the backend's `stanza` config must be no
higher than the version supported by the installed client library.

## The Go bindings

Older versions of this backend (prior to its removal from upstream Vault)
required a separate, Mono-dependent code-generation step
(`fdb-go-install.sh`) to produce the Go bindings locally. That is no longer
necessary: `github.com/apple/foundationdb/bindings/go` is a normal,
pre-generated Go module and is declared as a regular (if unused-by-default)
dependency in this repository's `go.mod`. `go build -tags foundationdb`
picks it up automatically, as long as the native client library and
headers from the previous step are discoverable by cgo (by default via
`/usr/local/include` and `/usr/local/lib` on Linux/macOS; override with
`CGO_CFLAGS`/`CGO_LDFLAGS` if installed elsewhere).

## Building OpenBao with the FoundationDB backend

```
$ CGO_ENABLED=1 go build -tags foundationdb -o bin/bao .
```

or, via `make`:

```
$ make dev FDB_ENABLED=1
```

## Running tests

```
$ CGO_ENABLED=1 go test -tags foundationdb ./internal/physical/foundationdb/...
```

The test suite starts a disposable, single-node FoundationDB cluster in
Docker (see `internal/helper/testhelpers/foundationdb`), building a
throwaway image from the official client/server `.deb` release assets,
unless `FOUNDATIONDB_CLUSTER_FILE` is set to point at an already-running
cluster.
