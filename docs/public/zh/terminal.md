---
title: 终端
description: 在终端视图中运行本地程序或远程字节流。
group: Components
order: 38
---

`base/terminal` 在 macOS、Windows 和 Linux 上运行本地程序，也可通过 `Options.Conn` 在这些平台及 iOS 上运行远程会话。应用负责会话的生命周期，结束时调用 `Close`：

```go
func Terminal(cx *core.Scope) core.Node {
	session, err := terminal.New(terminal.Options{})
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

远程会话将实现 `io.ReadWriteCloser` 的对象传给 `Conn`；终端读取它的输出，并写入编码后的键盘和鼠标输入。可选的 `Resize(cols, rows int) error` 或 `ResizePixels(cols, rows, cellWidth, cellHeight int) error` 方法接收视图尺寸。

程序启用鼠标跟踪后，会按所请求的模式接收按下、释放、拖动和悬停事件，由 libghostty-vt 按它选择的协议编码。按住 Shift 可绕过点击和拖动捕获以选择文字。`View.MouseReporting` 是可选的响应式 `func() bool`，nil 表示 true；返回 false 后，即使程序请求捕获，也会保留视图的文字选择和原生回滚区滚动。启用上报时，程序的右键事件优先于外层上下文菜单。

备用屏幕启用 alternate scroll 且未请求鼠标跟踪时，纵向滚轮输入会变成方向键，并跟随普通或应用方向键模式。`MouseScrollMultiplier{Precision: 1, Discrete: 3}` 为触控板和滚轮设置默认上报倍数；零值采用这些默认值，正数限制在 0.01 到 10,000。小数输入会积累到完整步数后发出。普通回滚区继续使用平台原生滚动和惯性。

导入 `tsx/terminal` 后可使用 `nsgo/base` 的 `<Terminal>`。`mouseReporting` 默认为 true；`mouseScrollPrecision` 和 `mouseScrollDiscrete` 设置倍数。

原始鼠标上报使用桌面指针事件。触屏选择和软件键盘保留各自的输入路径；Android 和浏览器终端运行时在路线图中单独跟踪。
