# sparql-server

A SPARQL 1.1 endpoint in a single Go binary. It serves the SPARQL 1.1
Protocol (query, update, service description) and the SPARQL 1.1 Graph Store
HTTP Protocol over an in-memory, Badger or SQLite store, with token
authentication, TLS, CORS, gzip, Prometheus metrics, health checks and
graceful shutdown.

The protocol handling, query engine, parsers and stores come from
[goRDFlib](https://github.com/tggo/goRDFlib) (its `endpoint` package passes
the W3C `protocol` and `http-rdf-update` test manifests). This repository is
the deployable part: command line, configuration, security, operations and
packaging.

- One static binary of about 20 MB, no JVM, no C libraries (CGO is off; SQLite
  is the pure-Go modernc port).
- No modules beyond goRDFlib (and what it already depends on) and the standard
  library.
- Updates are off until you turn them on.

## Contents

- [When to use it](#when-to-use-it)
- [Quick start](#quick-start)
- [HTTP interface](#http-interface)
- [Examples with curl](#examples-with-curl)
- [Commands](#commands)
- [Configuration](#configuration)
- [Security model](#security-model)
- [Storage](#storage)
- [Metrics and logs](#metrics-and-logs)
- [Limitations](#limitations)
- [Development](#development)

## When to use it

Compared with the established servers, in honest terms:

| | sparql-server | Apache Jena Fuseki | Oxigraph server | GraphDB |
|---|---|---|---|---|
| Runtime | one Go binary | JVM | one Rust binary | JVM |
| Storage | memory, Badger, SQLite | memory, TDB2 | memory, RocksDB | own engine |
| SPARQL 1.1 query | yes, except DESCRIBE | complete | complete | complete |
| SPARQL 1.1 update | yes, not atomic across operations | transactional | transactional | transactional |
| Graph Store Protocol | yes, no PATCH | yes | yes | yes |
| Reasoning, full-text search, federation (SERVICE) | no | yes (Jena modules) | no reasoning | yes |
| Admin UI | a query page | full UI | query page | Workbench |
| Maturity | new | many years in production | mature | commercial product |

Pick sparql-server when you want a small, dependency-free endpoint for small
and medium datasets (thousands to a few million triples), a Go-native
deployment, or an endpoint embedded in a Go stack (the same handler is a
library: `github.com/tggo/goRDFlib/endpoint`). Pick Fuseki, Oxigraph or
GraphDB when you need large datasets with fast bulk loads, transactional
updates, DESCRIBE, reasoning or text search.

## Quick start

### Binary

```sh
go install github.com/tggo/sparql-server/cmd/sparql-server@latest
# or download a release archive from https://github.com/tggo/sparql-server/releases

# Read-only, in memory, with the example data:
sparql-server serve --data 'examples/*'

# Persistent, with updates enabled by a token:
export SPARQL_SERVER_UPDATE_TOKEN=$(openssl rand -hex 32)
sparql-server serve --store badger:./data/badger --data examples/people.ttl
```

Open http://localhost:8080/ for the query page, or use curl (below).

### Docker

```sh
docker build -t sparql-server .
docker run --rm -p 8080:8080 -v "$PWD/examples:/examples:ro" \
  -e SPARQL_SERVER_DATA=/examples sparql-server

# Persistent store on a volume, updates enabled:
docker run -d -p 8080:8080 -v sparql-data:/data \
  -e SPARQL_SERVER_STORE=badger:/data/badger \
  -e SPARQL_SERVER_UPDATE_TOKEN="$(openssl rand -hex 32)" \
  sparql-server
```

The image is distroless, runs as user 65532 and has a writable `/data`.
Release tags also publish `ghcr.io/tggo/sparql-server`. See
[docker-compose.yml](docker-compose.yml) for a compose setup with a Badger
volume.

## HTTP interface

| Path | What it serves |
|---|---|
| `/sparql` | SPARQL queries (GET, POST form, POST `application/sparql-query`), updates (POST form, POST `application/sparql-update`), the service description (GET without a query), and Graph Store requests with `?graph=<iri>` or `?default` |
| `/graph-store` | Graph Store HTTP Protocol. `?graph=<iri>` and `?default` address graphs indirectly; `/graph-store/<anything>` addresses the graph whose IRI is that URL; POST to `/graph-store` creates a graph and returns its `Location` |
| `/<dataset>` and `/<dataset>/query`, `/<dataset>/sparql`, `/<dataset>/update`, `/<dataset>/data`, `/<dataset>/get` | Fuseki-compatible routes, only with `--dataset <dataset>`. `/<dataset>` behaves like `/sparql`; `query` and `sparql` accept only queries, `update` only updates, `data` is the Graph Store, `get` is the Graph Store for reading |
| `/healthz` | 200 while the process runs (liveness) |
| `/readyz` | 200 once the startup data is loaded, 503 before that and during shutdown (readiness) |
| `/metrics` | Prometheus text format (disable with `--metrics=false`) |
| `/` | query page (disable with `--ui=false`) |

Until the startup data is loaded, the SPARQL and Graph Store routes answer 503
with `Retry-After`, so a load balancer never sends queries to a half-loaded
server.

Result formats: SPARQL JSON, XML, CSV and TSV for SELECT and ASK; Turtle,
N-Triples, N-Quads, TriG, JSON-LD and RDF/XML for CONSTRUCT and graph reads,
chosen by the `Accept` header.

Status codes follow the goRDFlib endpoint: 400 malformed request or failed
update, 401 missing or wrong token, 403 not allowed (read-only, query token
used for writing, LOAD disabled), 404 unknown graph, 406 no acceptable format,
413 body too large, 415 unsupported media type, 422 result over
`--max-result-rows`, 501 DESCRIBE, 503 time limit exceeded or still loading.
Error bodies are `text/plain`.

## Examples with curl

```sh
EP=http://localhost:8080
TOKEN=$SPARQL_SERVER_UPDATE_TOKEN

# SELECT as JSON (GET)
curl -G $EP/sparql -H 'Accept: application/sparql-results+json' \
  --data-urlencode 'query=SELECT ?name WHERE { ?p <http://xmlns.com/foaf/0.1/name> ?name }'

# SELECT as CSV (POST form)
curl $EP/sparql -H 'Accept: text/csv' \
  --data-urlencode 'query=SELECT ?name WHERE { ?p <http://xmlns.com/foaf/0.1/name> ?name }'

# Query in the body
curl $EP/sparql -H 'Content-Type: application/sparql-query' -H 'Accept: text/tab-separated-values' \
  --data-binary 'SELECT (COUNT(*) AS ?n) WHERE { ?s ?p ?o }'

# CONSTRUCT as Turtle
curl -G $EP/sparql -H 'Accept: text/turtle' \
  --data-urlencode 'query=CONSTRUCT WHERE { ?s <http://xmlns.com/foaf/0.1/knows> ?o }'

# Named graphs
curl -G $EP/sparql -H 'Accept: text/csv' \
  --data-urlencode 'query=SELECT ?g (COUNT(*) AS ?n) WHERE { GRAPH ?g { ?s ?p ?o } } GROUP BY ?g'

# Service description
curl $EP/sparql -H 'Accept: text/turtle'

# Update (needs the update token)
curl $EP/sparql -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/sparql-update' \
  --data-binary 'INSERT DATA { GRAPH <http://example.org/g> { <http://example.org/a> <http://example.org/p> "x" } }'

# Update as a form, with Basic authentication (any user name, the token as password)
curl $EP/sparql -u "any:$TOKEN" --data-urlencode 'update=DELETE WHERE { ?s <http://example.org/p> ?o }'

# Graph Store: replace, read, add to, delete a graph
curl -X PUT "$EP/graph-store?graph=http://example.org/g" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: text/turtle' --data-binary @examples/people.ttl
curl "$EP/graph-store?graph=http://example.org/g" -H 'Accept: application/n-triples'
curl -X POST "$EP/graph-store?default" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/n-triples' --data-binary '<http://example.org/b> <http://example.org/p> "y" .'
curl -X DELETE "$EP/graph-store?graph=http://example.org/g" -H "Authorization: Bearer $TOKEN"

# Compressed response
curl --compressed -G $EP/sparql --data-urlencode 'query=SELECT * WHERE { ?s ?p ?o }'

# Operations
curl $EP/healthz
curl $EP/readyz
curl $EP/metrics
```

## Commands

```
sparql-server [serve] [flags]          run the server (the default)
sparql-server load [flags] <files...>  load RDF files into a persistent store
sparql-server query [flags] <query>    run a query against a store and print the result
sparql-server version                  print version information
```

`<command> -h` lists the flags. Flags come before positional arguments.

### load

```sh
sparql-server load --store badger:/var/lib/sparql/badger dump.nq.gz 'ontology/*.ttl' \
  --graph http://example.org/extra=extra.nt
# loaded 1204332 statements from 5 files in 41.2s; badger:/var/lib/sparql/badger now holds 1204332 triples
```

Files, directories (their RDF files, not recursive) and globs are accepted.
The format comes from the extension: `.ttl`, `.nt`, `.nq`, `.trig`, `.rdf`,
`.owl`, `.xml`, `.jsonld`, `.json`, each optionally followed by `.gz`.
N-Quads and TriG keep their graphs; other formats go to the default graph, or
to the graph named with `--graph <iri>=<file>`. N-Triples and N-Quads are
streamed in batches of 10 000; the other formats are parsed completely before
anything is written, so a malformed file adds nothing.

A Badger store can be opened by one process at a time: stop the server before
running `load` or `query` on its store. SQLite allows it but serializes writes.

### query

```sh
sparql-server query --store sqlite:./data.db 'SELECT * WHERE { ?s ?p ?o } LIMIT 10'
sparql-server query --data 'examples/*' --format json --file report.rq
echo 'ASK { ?s ?p ?o }' | sparql-server query --store badger:./badger -
```

`--format` is `auto` (TSV for SELECT and ASK, Turtle for CONSTRUCT), `json`,
`xml`, `csv`, `tsv`, `turtle`, `ntriples`, `nquads`, `trig`, `jsonld` or
`rdfxml`. The query runs in-process through the same handler as the server,
read-only. The exit code is 0 on success and 1 on any error (message on
stderr).

## Configuration

Every flag has an environment variable: the flag name in upper case with `-`
replaced by `_` and prefixed with `SPARQL_SERVER_`. A flag given on the command
line wins over the environment, the environment wins over the default.
Repeatable flags take a comma-separated list from the environment. Byte sizes
accept `KiB`, `MiB`, `GiB` (and `KB`, `MB`, `GB` in powers of 1000). An unknown
`SPARQL_SERVER_*` variable is reported at startup.

There is no configuration file: flags and environment variables cover
containers, systemd units and shells without adding a parser dependency.

### serve

| Flag | Environment variable | Default | Meaning |
|---|---|---|---|
| `--listen` | `SPARQL_SERVER_LISTEN` | `:8080` | address to listen on |
| `--store` | `SPARQL_SERVER_STORE` | `memory` | `memory`, `badger:/dir` or `sqlite:/file.db` |
| `--data` | `SPARQL_SERVER_DATA` | | file, directory or glob loaded at startup (repeatable) |
| `--graph` | `SPARQL_SERVER_GRAPH` | | `<iri>=<file>`: load a file into a named graph (repeatable) |
| `--base-iri` | `SPARQL_SERVER_BASE_IRI` | | base IRI for loaded files, queries and updates without BASE |
| `--public-url` | `SPARQL_SERVER_PUBLIC_URL` | | public `scheme://host[:port]` behind a reverse proxy; used for request IRIs (direct graph identification, Location) |
| `--dataset` | `SPARQL_SERVER_DATASET` | | also serve Fuseki-style routes under `/<dataset>` |
| `--update-token` | `SPARQL_SERVER_UPDATE_TOKEN` | | token that enables updates and Graph Store writes (16+ characters) |
| `--update-token-file` | `SPARQL_SERVER_UPDATE_TOKEN_FILE` | | read the update token from a file (Docker/Kubernetes secrets) |
| `--query-token` | `SPARQL_SERVER_QUERY_TOKEN` | | token required for queries and graph reads |
| `--query-token-file` | `SPARQL_SERVER_QUERY_TOKEN_FILE` | | read the query token from a file |
| `--allow-anonymous-updates` | `SPARQL_SERVER_ALLOW_ANONYMOUS_UPDATES` | `false` | allow writes without a token (local use only) |
| `--allow-load` | `SPARQL_SERVER_ALLOW_LOAD` | | enable SPARQL LOAD for URLs under this http(s) prefix (repeatable) |
| `--load-timeout` | `SPARQL_SERVER_LOAD_TIMEOUT` | `30s` | time limit for fetching one LOAD document |
| `--load-max-bytes` | `SPARQL_SERVER_LOAD_MAX_BYTES` | `64MiB` | size limit of one LOAD document |
| `--cors-origin` | `SPARQL_SERVER_CORS_ORIGIN` | | allowed CORS origin, exact, or `*` alone (repeatable) |
| `--tls-cert` | `SPARQL_SERVER_TLS_CERT` | | TLS certificate (PEM) |
| `--tls-key` | `SPARQL_SERVER_TLS_KEY` | | TLS private key (PEM) |
| `--query-timeout` | `SPARQL_SERVER_QUERY_TIMEOUT` | `30s` | time limit for one query, 0 for none |
| `--update-timeout` | `SPARQL_SERVER_UPDATE_TIMEOUT` | `1m` | time limit for one update or Graph Store write, 0 for none |
| `--max-request-bytes` | `SPARQL_SERVER_MAX_REQUEST_BYTES` | `10MiB` | size limit of a query or update body |
| `--max-graph-bytes` | `SPARQL_SERVER_MAX_GRAPH_BYTES` | `64MiB` | size limit of a Graph Store PUT or POST body |
| `--max-result-rows` | `SPARQL_SERVER_MAX_RESULT_ROWS` | `0` | fail with 422 above this many rows or triples, 0 for no limit |
| `--read-header-timeout` | `SPARQL_SERVER_READ_HEADER_TIMEOUT` | `10s` | time limit for request headers |
| `--read-timeout` | `SPARQL_SERVER_READ_TIMEOUT` | `5m` | time limit for reading a whole request |
| `--write-timeout` | `SPARQL_SERVER_WRITE_TIMEOUT` | `10m` | time limit for writing a response |
| `--idle-timeout` | `SPARQL_SERVER_IDLE_TIMEOUT` | `2m` | keep-alive idle timeout |
| `--max-header-bytes` | `SPARQL_SERVER_MAX_HEADER_BYTES` | `64KiB` | size limit of request headers; send long queries with POST |
| `--shutdown-timeout` | `SPARQL_SERVER_SHUTDOWN_TIMEOUT` | `30s` | drain time for running requests on SIGINT/SIGTERM |
| `--gzip` | `SPARQL_SERVER_GZIP` | `true` | gzip responses of 1 KiB and more for clients that accept it |
| `--metrics` | `SPARQL_SERVER_METRICS` | `true` | serve `/metrics` |
| `--ui` | `SPARQL_SERVER_UI` | `true` | serve the query page at `/` |
| `--access-log` | `SPARQL_SERVER_ACCESS_LOG` | `true` | one log line per SPARQL and Graph Store request |
| `--log-format` | `SPARQL_SERVER_LOG_FORMAT` | `text` | `text` or `json` |
| `--log-level` | `SPARQL_SERVER_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |

### load and query

`load` takes `--store` (a persistent one), `--graph`, `--base-iri`,
`--log-format` and `--log-level`. `query` takes `--store`, `--data`, `--graph`,
`--base-iri`, `--format`, `--file`, `--query-timeout`, `--log-format` and
`--log-level`. The environment variables are the same as for `serve`.

## Security model

- **Writes are disabled by default.** Without `--update-token` or
  `--allow-anonymous-updates` the endpoint is read-only: updates and Graph
  Store writes get 403 and the service description does not advertise
  SPARQL Update. `--allow-anonymous-updates` logs a warning at startup.
- **Tokens.** Send `Authorization: Bearer <token>`, or HTTP Basic with the
  token as the password (any user name) for clients that only speak Basic.
  The update token allows everything; the query token, when set, is required
  for queries and graph reads and does not allow writes (403). A missing or
  wrong token gets 401 with `WWW-Authenticate` challenges for Bearer and
  Basic. Tokens are compared in constant time over their SHA-256 digests,
  must be at least 16 characters, and are never logged.
- **Unauthenticated routes.** `/healthz`, `/readyz`, `/metrics` and the query
  page need no token. The metrics reveal request counts and the dataset size,
  not data; turn them off with `--metrics=false` or keep the port private.
- **LOAD.** SPARQL `LOAD` makes the server fetch a URL chosen by the client,
  so it is disabled (403) unless `--allow-load` lists URL prefixes. A URL is
  allowed when scheme, host and port equal a prefix and the path is the prefix
  path or below it at a segment boundary. Redirects are checked against the
  same list; dot segments, encoded slashes and user info are refused; local
  files are never read. Each document is limited by `--load-timeout` and
  `--load-max-bytes`. JSON-LD documents never fetch remote `@context`s, in
  LOAD, Graph Store bodies or startup files.
- **CORS.** Off unless `--cors-origin` is given. Origins match exactly; `*`
  is allowed only alone. `Access-Control-Allow-Credentials` is never sent:
  browser clients send the token in the `Authorization` header, which does not
  need credentialed requests.
- **TLS.** `--tls-cert` and `--tls-key` serve HTTPS with TLS 1.2 or newer. A
  warning is logged when tokens are configured on a non-loopback address
  without TLS; terminating TLS in a reverse proxy is fine (set `--public-url`).
  Certificates are read at startup; restart to rotate them.
- **Limits.** Header size, body sizes, header/read/write/idle timeouts, query
  and update time limits (503 when exceeded) and an optional result row limit
  (422, never a truncated result). Panics in request handling are recovered
  and logged without being sent to the client.
- **Query page.** One HTML file, one script and one stylesheet embedded in
  the binary, no external resources, served with
  `Content-Security-Policy: default-src 'none'; script-src 'self'; ...` and
  `X-Frame-Options: DENY`. Results are inserted as text, never as HTML. A
  token typed into it stays in the page and is sent only to this server.

## Storage

| `--store` | Persistence | Notes |
|---|---|---|
| `memory` | none | fastest; the dataset is gone when the process exits |
| `badger:/dir` | yes | Badger v4 LSM store; one process at a time; **recommended for persistent, query-heavy use** |
| `sqlite:/file.db` | yes | pure-Go SQLite in WAL mode; one file, inspectable with the `sqlite3` CLI; **much slower for queries** (see below) |

Query throughput on the same test (16 clients, a selective two-pattern query
over 20 000 triples, Apple M4 Max, loopback, `TestConcurrentQueryThroughput`):

| Store | Requests per second |
|---|---:|
| memory | ~28 000 |
| Badger | ~14 600 |
| SQLite | ~250 |

Pick memory when the data fits in RAM and can be reloaded at startup, Badger
when it must persist. Use SQLite only when a single inspectable file matters
more than query speed.

All three hold the default graph and any number of named graphs in one store.
A named graph exists while it holds a triple: an emptied graph disappears.

`--data` loads its files on every start. On a persistent store that is
harmless for triples without blank nodes (a store is a set), but blank nodes
get fresh identifiers on each load and accumulate. For persistent data run
`sparql-server load` once and start the server without `--data`.

The server keeps the store open while it runs and closes it on shutdown after
the running requests finish, so data written by acknowledged updates is on
disk after a clean stop.

## Metrics and logs

`/metrics` exposes, in Prometheus text format 0.0.4:

| Metric | Type | Labels |
|---|---|---|
| `sparql_server_requests_total` | counter | `op` (`query`, `update`, `graph-read`, `graph-write`, `unknown`), `status` |
| `sparql_server_request_duration_seconds` | histogram | `op` |
| `sparql_server_result_rows_total` | counter | `op`; solutions or triples of successful requests |
| `sparql_server_response_bytes_total` | counter | `op`; before compression |
| `sparql_server_in_flight_requests` | gauge | |
| `sparql_server_ready` | gauge | |
| `sparql_server_dataset_triples` | gauge | recounted only after writes |
| `sparql_server_build_info` | gauge | `version`, `goversion` |
| `sparql_server_start_time_seconds`, `sparql_server_goroutines` | gauge | |

The exposition is written by hand rather than with `prometheus/client_golang`,
which would add several modules (client_model, common, procfs, protobuf and more) for a few counters and one histogram.

Logs go to stderr through `log/slog`, as text or JSON. Every SPARQL and Graph
Store request produces one line:

```
level=INFO msg=request method=POST path=/sparql op=update status=401 duration_ms=0.019 rows=0 bytes=33 remote=127.0.0.1:56077 err="updates require the update token"
```

Query text is not logged. Server errors and recovered panics are logged at
error level, time limits at warn, client errors at debug.

On SIGINT or SIGTERM the server marks itself not ready, stops accepting
connections, lets running requests finish for `--shutdown-timeout`, cancels
the ones still running, and closes the store. A second signal exits at once.

## Limitations

- **DESCRIBE** is not implemented by the goRDFlib engine and returns 501.
- **Updates are serializable but not atomic.** Queries never see a
  half-applied update and updates never interleave, but a request with several
  operations whose third one fails (or times out) keeps the first two. There
  is no rollback.
- **Graph Store PATCH** is not supported (405).
- **Empty named graphs** are not kept (a graph exists while it has triples).
- **Evaluation is in memory.** The engine materializes solutions, so
  `--max-result-rows` bounds the response, not the memory a query uses; use
  `--query-timeout` for that. A single store call (a transitive property path
  pushed down to the store) is not interrupted by a timeout.
- **No reasoning, full-text search, SERVICE federation or GeoSPARQL.**
- **FROM with an undeclared prefix** is ignored rather than guessed (FROM is
  read by a lexical scanner).
- **Scale.** Bulk loading into Badger or SQLite goes through the generic store
  interface; expect minutes, not seconds, for tens of millions of triples.
  Query throughput per store is in the [Storage](#storage) table; SQLite is
  two orders of magnitude slower than memory.
- **Windows** binaries are built and cross-compiled in CI, but the test suite
  runs on Linux and macOS only.
- TLS certificates are not reloaded without a restart.

## Development

```sh
go vet ./...
go test -race -count=1 ./...
go test -run Throughput -v ./internal/cli        # the load test
go test -bench Query -run '^$' ./internal/cli    # the HTTP benchmark
go build -o sparql-server ./cmd/sparql-server
docker build -t sparql-server .
```

Layout: `cmd/sparql-server` is the entry point; `internal/cli` the commands
and the end-to-end tests; `internal/config` flags and environment;
`internal/server` the wiring; `internal/storage` and `internal/rdfio` stores
and file loading; `internal/auth` tokens; `internal/loader` the LOAD
allowlist; `internal/httpx` gzip, CORS and the readiness gate;
`internal/metrics` the exposition; `internal/ui` the query page.

Releases: push a `v*` tag; the release workflow runs goreleaser (archives for
linux, darwin and windows on amd64 and arm64, with SHA-256 checksums) and
pushes a multi-arch image to `ghcr.io/tggo/sparql-server`.

## License

BSD 3-Clause, the same as goRDFlib. See [LICENSE](LICENSE).
