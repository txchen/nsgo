---
title: Terminal
description: Run a local program or a remote byte stream in a terminal view.
group: Components
order: 38
---

`base/terminal` runs local programs on macOS, Windows and Linux, and remote sessions through `Options.Conn` there and on iOS. The app owns the session and closes it when finished:

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

Pass an `io.ReadWriteCloser` as `Conn` for a remote session; the terminal reads its output and writes encoded keyboard and mouse input. An optional `Resize(cols, rows int) error` or `ResizePixels(cols, rows, cellWidth, cellHeight int) error` method receives the view's size.

Programs that enable mouse tracking receive the requested presses, releases, drags and hover, encoded by libghostty-vt in their selected protocol. Shift bypasses click and drag capture for selection. `View.MouseReporting` is an optional reactive `func() bool`, true when nil; return false to keep mouse selection and native scrollback scrolling even while a program asks for capture. A program's right click takes priority over an enclosing context menu while reporting is enabled.

On the alternate screen, with alternate scroll mode enabled and no mouse tracking requested, vertical wheel input becomes cursor keys, following normal or application cursor-key mode. `MouseScrollMultiplier{Precision: 1, Discrete: 3}` sets the default report counts for touchpads and wheels; zero uses those defaults, and positive values are clamped to 0.01–10,000. Fractional input accumulates until a complete step is available. The normal scrollback keeps the platform's native scrolling and inertia.

Import `tsx/terminal` to register `<Terminal>` in `nsgo/base`. Its `mouseReporting` prop defaults to true; `mouseScrollPrecision` and `mouseScrollDiscrete` set the multipliers.

Raw mouse reports use desktop pointer events. Touch-screen selection and the software keyboard keep their own input path; Android and browser terminal runtimes are tracked separately in the roadmap.
