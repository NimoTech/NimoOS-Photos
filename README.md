# NimoOS-Photos

The photo service for NimoOS — **indexing, EXIF parsing, thumbnails and semantic search** for photos and video.

> ### About
>
> NimoOS is a fork of [CasaOS](https://github.com/IceWhaleTech/CasaOS)
> (Apache-2.0), originally developed by IceWhale Technology Co., Ltd.
> Building on that foundation, NimoOS adds an AI agent, RAG-based retrieval,
> a knowledge layer, and a built-in web terminal.
>
> See [`NOTICE`](./NOTICE) for attribution details. CasaOS and IceWhale are
> trademarks of IceWhale Technology Co., Ltd.; NimoOS is an independent
> project and is not affiliated with IceWhale.
>
> This repository is NimoTech's own work and contains no CasaOS-derived code.


> ⚠️ Multi-user isolation is incomplete — Photos and Search are not yet
> per-user scoped. Read
> [SECURITY.md](https://github.com/NimoTech/NimoOS/blob/main/SECURITY.md#known-limitations)
> before deploying NimoOS for more than one person.


## What this is
Binds a random localhost port and is fronted by the NimoOS gateway under
`/v1/photos`, with resumable TUS uploads under `/v1/upload-tus`.

## Capabilities

| Capability | Notes |
|---|---|
| Indexing and EXIF | Photo and video metadata |
| Thumbnails | Generated and cached on demand |
| Semantic search | Local vector search (sqlite-vec), natural-language queries |
| Captioning | Local vision model |
| Resumable uploads | TUS protocol |

> ⚠️ **The photo library is currently global** — every user account sees the
> same albums, with no per-user filtering at the data layer. See the multi-user
> note above.


## Building

NimoOS is a multi-repository project. Every Go service uses a `replace`
directive pointing at the local `NimoOS-Common` checkout, so a build needs the
full workspace — see
[NimoOS-Build](https://github.com/NimoTech/NimoOS-Build) for the layout and the
one-line clone helper.

`NimoOS-MessageBus` must be generated first; its generated API code is not
committed and other services' `go generate` consumes its OpenAPI spec.

```bash
CGO_ENABLED=1 go build ./...   # SQLite + sqlite-vec, needs system sqlite3.h
go test ./...
```

Go services pin `go 1.21` and echo v4.12 — **do not run `go mod tidy`**.


## Documentation

Architecture, request flow and runtime details: [`OVERVIEW.md`](./OVERVIEW.md).

## License

Apache-2.0 — see [`LICENSE`](./LICENSE).
