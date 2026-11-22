---
title: 终端
description: 在终端视图中运行本地程序或远程字节流。
group: 组件
order: 38
---

`base/terminal` 在 macOS、Windows 和 Linux 上运行本地程序，也能通过 `Options.Conn` 在这些平台、iOS 和浏览器中连接远程会话。应用负责会话的生命周期，并在使用结束时关闭它：

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

终端读取连接中的输出，并向连接写入编码后的输入。连接可以实现 `Resize(cols, rows int) error` 或 `ResizePixels(cols, rows, cellWidth, cellHeight int) error`，接收视图的尺寸。服务器决定网格大小并通过 `Resize` 或 `ResizePixels` 回传时，设置 `ResizeByConn`。`Close` 会关闭连接，连接中阻塞的读取必须随之返回。

运行本地程序时，不设置 `Conn`，通过 `Command`、`Dir` 和 `Env` 配置程序；命令为空时启动用户的 shell。iOS 和浏览器会话必须提供 `Conn`。Android、HarmonyOS 和小程序的终端支持仍是路线图中的目标。

## 在浏览器中

应用内嵌固定版本的 libghostty-vt WebAssembly 模块，使用相同的终端视图和输入路径。构建应用无需 Zig、额外安装原生库，也不需要在运行时下载库。

将浏览器的 WebSocket 适配为 `io.ReadWriteCloser`：发送二进制字节，并把收到的消息合并或拆分为连续字节流。认证、重连以及尺寸和控制消息由应用与服务器决定；NSGo 只处理终端字节。[浏览器验收用例](https://github.com/nsgo-dev/nsgo/blob/main/internal/testhost/testdata/terminalweb/socket_js_test.go) 提供了包含关闭处理的完整字节流适配器。

通过 `View.Font` 设置等宽字体和字号，通过 `Theme`、`DarkTheme` 设置调色板，通过 `Ref` 在应用菜单中提供选择与剪贴板操作。浏览器可以使用内置的 Ghostty 主题；应用自带的主题文本应交给 `ParseGhosttyTheme`，而不是使用原生配置文件路径。

导入 `tsx/terminal`，将 `<Terminal>` 注册到 `nsgo/base`；其中的 `Conn` 遵循相同的 Go 字节流约定。
