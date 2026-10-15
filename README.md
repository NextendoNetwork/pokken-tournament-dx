# Pokken Tournament DX - Nextendo server

Nextendo Network server for Pokken Tournament DX, written in Go on top of [nextendo-nex](https://github.com/NextendoNetwork/nextendo-nex).

## Build

Clone `nextendo-nex` next to this repository as `nextendo-nex-main` (see the `replace` line in `go.mod`), then run `go build .`. Configuration comes from environment variables; secrets are never stored in this repository.

## Disclaimer

This server ships **no** Nintendo code, keys, or copyrighted assets. It is an independent
reimplementation for use with a community-run replacement service, not affiliated with, endorsed by,
or associated with Nintendo or the game's publisher.

## License

Released under the **[PolyForm Shield License 1.0.0](LICENSE.md)**, source-available: read, use,
modify, and self-host, but do not use it to provide a product that competes with Nextendo Network.
