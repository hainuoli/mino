<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/hainuoli/mino-doc/main/public/brand/svg/mino-wordmark-white.svg">
    <img src="https://raw.githubusercontent.com/hainuoli/mino-doc/main/public/brand/svg/mino-wordmark-black.svg" alt="Mino" width="280">
  </picture>
</p>
<p align="center">
  <strong>从一个包开始，随项目成长。</strong><br>
  渐进式 Go 开发库。小巧的包，熟悉的 API。
</p>
<p align="center">
  <a href="README.md">English</a>
  &nbsp;·&nbsp;
  <a href="README.zh.md">简体中文</a>
  &nbsp;·&nbsp;
  <a href="https://github.com/hainuoli/mino-doc">使用文档</a>
  &nbsp;·&nbsp;
  <a href="https://pkg.go.dev/github.com/hainuoli/mino">API</a>
</p>

<table align="center">
  <tr>
    <td width="50%" valign="top"><strong>渐进式成长</strong><br>从一个工具包开始，随项目逐步扩展。</td>
    <td width="50%" valign="top"><strong>按需引入</strong><br>各包独立可用，需要时自由组合。</td>
  </tr>
  <tr>
    <td width="50%" valign="top"><strong>轻量小巧</strong><br>精简的实现，运行时仅依赖 Go 标准库。</td>
    <td width="50%" valign="top"><strong>快速高效</strong><br>高效的路由匹配，低开销的中间件组合。</td>
  </tr>
  <tr>
    <td width="50%" valign="top"><strong>符合直觉</strong><br>标准 HTTP 类型，熟悉的上下文与错误链。</td>
    <td width="50%" valign="top"><strong>愉悦的 DX</strong><br>简洁一致的 API，顺手完成绑定、验证与响应。</td>
  </tr>
</table>

## 快速开始

需要 Go 1.23+。

```sh
go get github.com/hainuoli/mino/rest
```

```go
mux := rest.NewMux()

mux.Get("/", func(w http.ResponseWriter, r *http.Request) {
	httpx.Ok(w, "Hello, Mino!")
})

http.ListenAndServe(":8080", mux)
```

[运行完整示例 →](https://github.com/hainuoli/mino-doc/blob/main/content/docs/zh/getting-started.mdx) · [了解渐进式使用 →](https://github.com/hainuoli/mino-doc/blob/main/content/docs/zh/progressive-adoption.mdx)

## 从标准库平滑迁移

替换 `http.ServeMux` 构造器，保留原来的注册方式。

```diff
- mux := http.NewServeMux()
+ mux := rest.NewMux()

 mux.HandleFunc("GET /users/{id}", getUser)
```

handler、中间件和 `r.PathValue` 都能继续使用。路径清理、转义匹配、尾斜杠重定向，按原服务需要开启。

[ServeMux 迁移指南 →](https://github.com/hainuoli/mino-doc/blob/main/content/docs/zh/migration.mdx)

## 性能

耗时中位数，单位 **ns/op**；越低越好。

| 场景 | Mino | Gin | Chi | go-zero router |
| --- | ---: | ---: | ---: | ---: |
| 静态路由 | **85.5** | 101.2 | 220.3 | 123.4 |
| 单路径参数 | 183.0 | **127.0** | 343.5 | 385.2 |
| 三个路径参数 | 274.5 | **197.6** | 499.1 | 679.2 |
| 1,000 条静态路由 | **131.3** | 170.6 | 355.1 | 5,328.0 |
| 五层中间件 | **254.9** | 279.1 | 410.7 | 304.4 |
| 路由 + JSON 响应 | **178.5** | 217.9 | 316.5 | 219.5 |

Apple M4 · macOS arm64 · Go 1.25.4 · 2026-11-15。五次、每次 1 秒，`-cpu=1`。

<details>
<summary>测试方法与参数路由开销</summary>

Gin v1.12.0 · Chi v5.5.0 · go-zero v1.10.3。

进程内 `ServeHTTP` 微基准，响应逻辑相同，均无默认中间件。go-zero 使用独立路由器。不含网络与业务开销。

Mino 通过 `Request.SetPathValue` 写入参数，让 handler 继续使用 `r.PathValue`。这些新请求在 Go 1.25.4 下会初始化参数 map；Gin 复用池化参数切片，因此 Mino 多出参数存储的分配。

[测试方法、分配分析与原始数据 →](https://github.com/hainuoli/mino-doc/blob/main/content/docs/zh/benchmarks.mdx)

</details>

## 轻量

**86.6 KiB 源码，零第三方运行时依赖。**

全部六个包共 **2,003 行生产代码**。在最小 HTTP 服务中，Mino 路由比标准库基线仅增加 **88 KiB**。

<details>
<summary>源码与二进制测量</summary>

| 生产源码 | 代码行数 | 体积 |
| --- | ---: | ---: |
| 核心工具 | 283 | 11.7 KiB |
| 路由与中间件组合 | 1,417 | 59.8 KiB |
| HTTP 辅助层 | 303 | 15.2 KiB |
| **全部六个包** | **2,003** | **86.6 KiB** |

代码行数排除注释与空行，源码体积包含注释与空白。两项均不计入测试、示例和工具代码。

| 最小 HTTP 服务 | 二进制体积 |
| --- | ---: |
| `net/http` 基线 | 5.30 MiB |
| Mino 路由 | 5.39 MiB |

Go 1.25.4 · linux/amd64 · CGO 关闭 · `-trimpath -ldflags='-s -w'`。两者均包含 Go runtime。

[测量程序与详细结果 →](https://github.com/hainuoli/mino-doc/blob/main/content/docs/zh/benchmarks.mdx)

</details>

## 继续探索

[文件上传与中间件](docs/uploads.md) · [上传示例](examples/upload/main.go)

[快速开始](https://github.com/hainuoli/mino-doc/blob/main/content/docs/zh/getting-started.mdx) · [包与默认行为](https://github.com/hainuoli/mino-doc/blob/main/content/docs/zh/packages.mdx) · [指南与示例](https://github.com/hainuoli/mino-doc) · [API 文档](https://pkg.go.dev/github.com/hainuoli/mino)

[贡献指南](CONTRIBUTING.md) · [安全报告](SECURITY.md) · [MIT 许可证](LICENSE)
