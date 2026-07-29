# NimoOS-Photos

NimoOS 的相册服务 —— **照片/视频索引、EXIF 解析、缩略图生成、语义搜索**。

> ### About / 关于本项目
>
> NimoOS is a fork of [CasaOS](https://github.com/IceWhaleTech/CasaOS)
> (Apache-2.0), originally developed by IceWhale Technology Co., Ltd.
> Building on that foundation, NimoOS adds an AI agent, RAG-based
> retrieval, a knowledge layer, and a built-in web terminal.
>
> NimoOS 基于 [CasaOS](https://github.com/IceWhaleTech/CasaOS)（Apache-2.0）
> fork 而来，原始项目由 IceWhale Technology Co., Ltd. 开发。在此基础上，
> NimoOS 重建了 AI Agent、RAG 检索、知识库与内置终端等能力。
>
> 归属详情见 [`NOTICE`](./NOTICE)。CasaOS 与 IceWhale 是 IceWhale Technology
> Co., Ltd. 的商标；NimoOS 是独立项目，与 IceWhale 无隶属关系。
>
> 本仓库是 NimoTech 原创，不含 CasaOS 衍生代码。

> ⚠️ Multi-user isolation is incomplete — Photos and Search are not yet
> per-user scoped. Read
> [SECURITY.md](https://github.com/NimoTech/NimoOS/blob/main/SECURITY.md#known-limitations)
> before deploying NimoOS for more than one person.
>
> ⚠️ 多用户隔离尚不完整（Photos 与搜索未按用户隔离）。若要给多人使用，请先阅读
> [SECURITY.md](https://github.com/NimoTech/NimoOS/blob/main/SECURITY.md#known-limitations)。

## 这是什么

绑定 localhost 随机端口、由 NimoOS Gateway 转发，API 前缀 `/v1/photos`；
TUS 断点续传上传前缀 `/v1/upload-tus`。

## 主要能力

| 能力 | 说明 |
|---|---|
| 索引与 EXIF | 照片 / 视频元数据解析 |
| 缩略图 | 按需生成与缓存 |
| 语义搜索 | 本地向量检索（sqlite-vec），支持自然语言搜图 |
| 图像描述 | 本地视觉模型生成 caption |
| 断点续传上传 | TUS 协议 |

> ⚠️ **相册库当前是全局共享的** —— 所有用户账号看到同一个库，数据层没有
> per-user 过滤。详见上方多用户提示与主仓 SECURITY.md。

## 构建

需要完整的 NimoOS monorepo checkout —— 所有 Go 服务通过 `replace` 指向本地的
`NimoOS-Common`，`go.mod` 里的版本号是装饰性的。

```bash
CGO_ENABLED=1 go build ./...   # SQLite + sqlite-vec，需要系统 sqlite3.h
go test ./...
```

## 文档

架构、请求流转与运行时细节见 [`OVERVIEW.md`](./OVERVIEW.md)。

## 许可

Apache-2.0，见 [`LICENSE`](./LICENSE)。
