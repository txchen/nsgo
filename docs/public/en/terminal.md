---
title: Terminal
description: Run a local program or a remote byte stream in a terminal view.
group: Components
order: 38
---

`base/terminal` runs local programs on macOS, Windows and Linux, and remote sessions through `Options.Conn` there, on iOS and in a browser. The app owns the session and closes it when finished:

```go
func RemoteTerminal(cx *core.Scope, conn io.ReadWriteCloser) core.Node {
	session, err := terminal.New(terminal.Options{Conn: conn})
	if err != nil {
		return core.Text{Content: err.Error()}
	}
	cx.Cleanup(func() { session.Close() })
	return terminal.View{
		Terminal: session,
		AutoFocus: true,
		Style: core.Style{Width: core.Pct(100), Height: core.Pct(100)},
	}
}
```

The terminal reads the connection's output and writes encoded input. An optional `Resize(cols, rows int) error` or `ResizePixels(cols, rows, cellWidth, cellHeight int) error` method receives the view's size. Set `ResizeByConn` when the server controls the terminal's grid size and reports it back through `Resize` or `ResizePixels`. `Close` closes the connection, whose blocked reads must then return.

For a local program, leave `Conn` empty and set `Command`, `Dir` and `Env`; an empty command starts the user's shell. iOS and browser sessions require `Conn`. Android, HarmonyOS and mini-program terminal support remain roadmap goals.

## In a browser

The app embeds the pinned libghostty-vt WebAssembly module and runs the same terminal view and input path. Building the app needs no Zig, native library installation or runtime download.

Adapt the browser's WebSocket to `io.ReadWriteCloser`: send binary bytes and join or split incoming messages as a stream. The app and its server own authentication, reconnecting and any resize/control messages; NSGo consumes terminal bytes. The [browser acceptance fixture](https://github.com/nsgo-dev/nsgo/blob/main/internal/testhost/testdata/terminalweb/socket_js_test.go) shows a complete byte-stream adapter with close handling.

Use `View.Font` for its fixed-pitch font and size, `Theme` and `DarkTheme` for its palettes, and `Ref` for selection and clipboard actions in the app's menus. Bundled Ghostty themes work in a browser; use `ParseGhosttyTheme` for an app's theme text instead of a native configuration-file path.

Import `tsx/terminal` to register `<Terminal>` in `nsgo/base`; its `Conn` is the same Go stream contract.
