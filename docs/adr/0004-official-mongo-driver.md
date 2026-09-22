# MongoDB 访问使用官方 mongo-driver,不走 GoFrame gdb

Status: Superseded by ADR-0005(数据存储改用 SQLite 单文件)

GoFrame 的 ORM(gdb)不支持 MongoDB,社区适配包增加依赖风险。决定直接使用 `go.mongodb.org/mongo-driver` 封装存储层(internal/store),GoFrame 仅承担 HTTP/WS/配置/日志。数据访问模式为文档存储直写直查 + 少量聚合管道,无需 ORM。

> 后续演进:ADR-0005 把存储从 MongoDB 换成 SQLite 单文件,「不上 ORM、直写直查」的结论保留;本文档中关于 mongo-driver 的部分已不再适用。
