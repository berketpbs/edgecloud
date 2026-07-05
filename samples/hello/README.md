# samples/hello

Minimal deployable [edgeCloud](https://github.com/poyrazK/edgecloud) FaaS
handler. For any inbound HTTP request it returns a small JSON document:

```json
{"hello":"world","path":"/the/request/path","now":1717689600000}
```

The point of the sample is to be the smallest possible end-to-end-deployable
guest component. It exists so the preview CI in
[`.github/workflows/preview.yml`](../../.github/workflows/preview.yml) has
a real artifact to upload on every PR, and so a new edgeCloud tenant has
something to fork and modify when they're learning the `wasi:http` guest
interface.

## Build

The two-step build is required because of a WIT-version mismatch between
the `wasm32-wasip2` target (which embeds `wit-component 0.241.x` and emits
`wasi:io@0.2.6` / `wasi:http/types@0.2.4`) and `wasmtime 45.0.3` (which
expects `wasi:io@0.2.1` / `wasi:http/types@0.2.1`). The matching
toolchain is `wasm32-unknown-unknown` core module + `wasm-tools component
new` wrapping with `--world edge-runtime-handler`.

```sh
cd samples/hello
cargo build --target wasm32-unknown-unknown --release
wasm-tools component new \
  target/wasm32-unknown-unknown/release/hello.wasm \
  --world edge-runtime-handler \
  -o target/component.wasm
```

The wrapped `target/component.wasm` is what `edge deploy` uploads. The
preview CI copies it to `target/wasm32-wasip2/release/hello.wasm` (the
path the CLI looks at by default) before invoking `edge deploy --preview`.

## Deploy

```sh
EDGE_API_KEY=... EDGE_API_URL=https://api.edgecloud.dev \
  edge deploy --preview
```

The CLI prints the deployed URL on its own line (`  URL: <url>`), which
the preview composite action captures and posts to the originating PR.

## Layout

```
samples/hello/
├── Cargo.toml         # crate-type = ["cdylib"], isolated [workspace]
├── edge.toml          # [project] name = "hello", [deployment] api = ...
├── README.md          # this file
├── src/
│   └── lib.rs         # wasi:http/incoming-handler implementation
└── wit/
    ├── edge-cloud.wit   # vendored from edge-worker/tests/fixtures/wit/
    └── deps/            # wasi:http, wasi:io, wasi:cli, ... @0.2.1
```

The `wit/` tree is vendored rather than relative-pathing into
`edge-worker/tests/fixtures/wit/` so the sample is self-contained —
moving or deleting the host repo's fixture path doesn't break the
sample.