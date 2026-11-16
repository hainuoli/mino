<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/hainuoli/mino-doc/main/public/brand/svg/mino-wordmark-white.svg">
    <img src="https://raw.githubusercontent.com/hainuoli/mino-doc/main/public/brand/svg/mino-wordmark-black.svg" alt="Mino" width="280">
  </picture>
</p>
<p align="center">
  <strong>Start with one package. Grow at your own pace.</strong><br>
  A progressive Go toolkit. Small packages, familiar APIs.
</p>
<p align="center">
  <a href="README.md">English</a>
  &nbsp;·&nbsp;
  <a href="README.zh.md">简体中文</a>
  &nbsp;·&nbsp;
  <a href="https://github.com/hainuoli/mino-doc">Documentation</a>
  &nbsp;·&nbsp;
  <a href="https://pkg.go.dev/github.com/hainuoli/mino">API</a>
</p>

<table align="center">
  <tr>
    <td width="50%" valign="top"><strong>Progressive</strong><br>Start with a utility package. Grow into a service.</td>
    <td width="50%" valign="top"><strong>Adopt on demand</strong><br>Use each package independently. Compose them as needed.</td>
  </tr>
  <tr>
    <td width="50%" valign="top"><strong>Lightweight</strong><br>A compact implementation. Only the Go standard library at runtime.</td>
    <td width="50%" valign="top"><strong>Fast</strong><br>Fast route matching. Low-overhead middleware composition.</td>
  </tr>
  <tr>
    <td width="50%" valign="top"><strong>Intuitive</strong><br>Standard HTTP types, familiar contexts, and Go error chains.</td>
    <td width="50%" valign="top"><strong>Delightful DX</strong><br>Clean, consistent APIs for binding, validation, and responses.</td>
  </tr>
</table>

## Quick start

Requires Go 1.23+.

```sh
go get github.com/hainuoli/mino/rest
```

```go
mux := rest.NewMux()

mux.Get("/", func(w http.ResponseWriter, r *http.Request) {
	httpx.OkJSON(w, "Hello, Mino!")
})

http.ListenAndServe(":8080", mux)
```

[Run the complete example →](https://github.com/hainuoli/mino-doc/blob/main/content/docs/en/getting-started.mdx) · [Explore progressive adoption →](https://github.com/hainuoli/mino-doc/blob/main/content/docs/en/progressive-adoption.mdx)

## Bring your existing handlers

Replace the `http.ServeMux` constructor and keep your registrations.

```diff
- mux := http.NewServeMux()
+ mux := rest.NewMux()

 mux.HandleFunc("GET /users/{id}", getUser)
```

Keep your handlers, middleware, and `r.PathValue`. Enable path cleaning, escaped-path matching, and slash redirects as your existing service requires.

[ServeMux migration guide →](https://github.com/hainuoli/mino-doc/blob/main/content/docs/en/migration.mdx)

## Performance

Median **ns/op**. Lower is better.

| Workload | Mino | Gin | Chi | go-zero router |
| --- | ---: | ---: | ---: | ---: |
| Static route | **85.5** | 101.2 | 220.3 | 123.4 |
| One path parameter | 183.0 | **127.0** | 343.5 | 385.2 |
| Three path parameters | 274.5 | **197.6** | 499.1 | 679.2 |
| 1,000 static routes | **131.3** | 170.6 | 355.1 | 5,328.0 |
| Five middleware layers | **254.9** | 279.1 | 410.7 | 304.4 |
| Route + JSON response | **178.5** | 217.9 | 316.5 | 219.5 |

Apple M4 · macOS arm64 · Go 1.25.4 · 2026-11-15. Five 1-second runs, `-cpu=1`.

<details>
<summary>Benchmark setup and parameter-route cost</summary>

Gin v1.12.0 · Chi v5.5.0 · go-zero v1.10.3.

In-process `ServeHTTP` benchmarks with identical response work and no default middleware. go-zero uses its standalone router. Network and business logic are excluded.

Mino writes parameters through `Request.SetPathValue` so handlers can use `r.PathValue`. For these fresh requests, Go 1.25.4 initializes a parameter map; Gin reuses a pooled parameter slice. This accounts for Mino's additional parameter-storage allocations.

[Methodology, allocation analysis, and raw results →](https://github.com/hainuoli/mino-doc/blob/main/content/docs/en/benchmarks.mdx)

</details>

## Small footprint

**86.6 KiB of source. Zero third-party runtime dependencies.**

All six packages total **2,003 production code lines**. In a minimal HTTP server, the Mino router adds **88 KiB** over the standard-library baseline.

<details>
<summary>Source and binary measurements</summary>

| Production source | Code lines | Size |
| --- | ---: | ---: |
| Core utilities | 283 | 11.7 KiB |
| Router + middleware composition | 1,417 | 59.8 KiB |
| HTTP helpers | 303 | 15.2 KiB |
| **All six packages** | **2,003** | **86.6 KiB** |

Code lines exclude comments and blank lines. Source size includes comments and whitespace. Tests, examples, and tools are excluded from both counts.

| Minimal HTTP server | Binary size |
| --- | ---: |
| `net/http` baseline | 5.30 MiB |
| Mino router | 5.39 MiB |

Go 1.25.4 · linux/amd64 · CGO disabled · `-trimpath -ldflags='-s -w'`. Both binaries include the Go runtime.

[Fixtures and measurement details →](https://github.com/hainuoli/mino-doc/blob/main/content/docs/en/benchmarks.mdx)

</details>

## Explore Mino

[Getting started](https://github.com/hainuoli/mino-doc/blob/main/content/docs/en/getting-started.mdx) · [Packages](https://github.com/hainuoli/mino-doc/blob/main/content/docs/en/packages.mdx) · [Guides and examples](https://github.com/hainuoli/mino-doc) · [API reference](https://pkg.go.dev/github.com/hainuoli/mino)

[Contributing](CONTRIBUTING.md) · [Security](SECURITY.md) · [MIT License](LICENSE)
