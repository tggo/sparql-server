# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.1.1] - 2026-09-15

### Changed

- Built on goRDFlib v0.5.5. Queries on a Badger store read from one snapshot
  instead of opening a transaction per lookup: about 3× the throughput
  (~4 800 → ~14 600 requests per second on the throughput test).
- README: query throughput for every store, and which store to choose. SQLite
  is much slower for queries (~250 requests per second on the same test).
- The throughput test also measures SQLite.

## [0.1.0] - 2026-09-15

First release, built on goRDFlib v0.5.4 and its `endpoint` package.

### Added

- `serve`: SPARQL 1.1 Protocol (query, update, service description) at
  `/sparql`, Graph Store HTTP Protocol at `/graph-store`, optional
  Fuseki-style routes with `--dataset`.
- Storage: in-memory, Badger (`badger:/dir`) and SQLite (`sqlite:/file.db`).
  Startup data with `--data` (files, directories, globs, `.gz`) and
  `--graph <iri>=<file>`; N-Quads and TriG keep their named graphs.
- `load`: bulk-load files into a persistent store.
- `query`: run a query against a store or files from the terminal, in any
  result or RDF format.
- `version`.
- Security defaults: updates and Graph Store writes are off unless
  `--update-token` or `--allow-anonymous-updates` is given; optional
  `--query-token`; Bearer or Basic credentials compared in constant time;
  SPARQL LOAD only from `--allow-load` URL prefixes; CORS with exact origins;
  TLS; request, graph, header, result-row and time limits.
- Operations: `/healthz`, `/readyz` (ready after the startup load),
  Prometheus metrics at `/metrics`, structured logs in text or JSON with one
  access log line per request, gzip, graceful shutdown on SIGINT/SIGTERM.
- An embedded query page at `/`.
- Every flag has a `SPARQL_SERVER_*` environment variable.
- Dockerfile (distroless, non-root), docker-compose example, goreleaser
  configuration, CI and release workflows.

[Unreleased]: https://github.com/tggo/sparql-server/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/tggo/sparql-server/releases/tag/v0.1.0
