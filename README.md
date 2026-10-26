# latai for Go

Run small open GGUF models on the device from Go: text, chat, JSON by schema, tool calls, speech
and images, through the latai runtime. The module binds latai's C interface
([`include/latai.h`](include/latai.h)) and loads the prebuilt core at run time, so `go build`
needs no core and no link flags.

```sh
go get github.com/hitl-systems/latai-go
go run github.com/hitl-systems/latai-go/cmd/latai-core   # once: fetch and check the core of this version
```

`latai-core` downloads the core archive pinned in [`core-pins.json`](core-pins.json) for your
platform (this release pins one: Linux x86_64, glibc 2.28 or newer; macOS, Windows and arm64 follow), checks its size and sha256 and every file
inside against its `core.json`, and unpacks it into the user cache. `latai.EnsureCore(ctx)` does the
same from your program. Set `LATAI_LIB` to use a core you put elsewhere.

```go
rt, err := latai.Open(&latai.Options{DataDir: "/path/to/data"})
if err != nil { log.Fatal(err) }
defer rt.Close()

if _, err := rt.Install("my-pack.gguf", true); err != nil { log.Fatal(err) }

resp, err := rt.Run(ctx, latai.TextRequest("my-pack", "Hello"))
fmt.Println(resp.Text())

for ev, err := range rt.Events(ctx, latai.TextRequest("my-pack", "Tell me more")) {
    if err != nil { log.Fatal(err) }
    if ev.Delta != nil { fmt.Print(ev.Delta.Text) }
}
```

- **Types** (`types_gen.go`) are generated from latai's wire schema: a tagged union such as
  `Inputs` or `Output` is a struct with one pointer per variant, exactly one set; a variant this
  version does not know is kept in `Unknown`.
- **Errors** are `*latai.Error` with `Code`, `Message`, `Hint`, `Details`, and the `Response` of a
  run that failed; `latai.HasCode(err, latai.CodeUnknownPack)`.
- **Cancel** by cancelling the context, or by breaking out of `Events`.
- **One runtime per process.** Its methods are safe for concurrent use. Do not call the runtime from
  inside a stream callback or an `Events` loop body except `Cancel`.

## Licences

The Go source of this module is Apache-2.0 OR MIT (`LICENSE-APACHE`, `LICENSE-MIT`). The latai
core it downloads is a prebuilt binary under its own licence (`LICENSE-BINARY` in the archive, with
`THIRD_PARTY_LICENSES` and `NOTICE`): free to use and to redistribute inside your software,
not to modify or reverse engineer.
