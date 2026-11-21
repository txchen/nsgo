package core

import (
	"errors"
	"testing"
	"time"

	"github.com/nsgo-dev/nsgo/internal/driver"
)

type fakeOutputFactory struct {
	ApproxTextEngine
	opened []*fakeAudioOutput
	err    error
}

func (f *fakeOutputFactory) OpenAudioOutput(c driver.AudioOutputConfig) (driver.AudioOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := &fakeAudioOutput{conf: c}
	f.opened = append(f.opened, out)
	return out, nil
}

type fakeAudioOutput struct {
	conf      driver.AudioOutputConfig
	started   int
	paused    int
	closed    int
	flushed   int
	frames    int64
	at        time.Time
	latency   time.Duration
	underruns int
}

func (o *fakeAudioOutput) Start() error                 { o.started++; return nil }
func (o *fakeAudioOutput) Pause()                       { o.paused++ }
func (o *fakeAudioOutput) Close()                       { o.closed++ }
func (o *fakeAudioOutput) Flush() int                   { o.flushed++; return 12 }
func (o *fakeAudioOutput) Position() (int64, time.Time) { return o.frames, o.at }
func (o *fakeAudioOutput) Latency() time.Duration       { return o.latency }
func (o *fakeAudioOutput) Underruns() int               { return o.underruns }

func TestNewAudioOutputWorksWithoutCanPlayAudioAndCleansUp(t *testing.T) {
	factory := &fakeOutputFactory{}
	var cx *Scope
	h := newHost(t, Size{Width: 100, Height: 100}, nil)
	h.tree = NewTree(h.root, factory)
	h.tree.Resize(Size{Width: 100, Height: 100}, Geometry{})
	h.tree.Mount(func(c *Scope) Node { cx = c; return View{} })

	// Env.CanPlayAudio is false (as on Linux without Sound/Player), yet a
	// platform offering AudioOutputFactory still opens pull PCM output.
	if Untracked(cx, cx.Env().CanPlayAudio) {
		t.Fatal("expected CanPlayAudio to default false")
	}

	child := cx.Child()
	var interrupts []error
	out, err := child.NewAudioOutput(AudioOutputConfig{
		SampleRate:  48000,
		Channels:    2,
		Latency:     10 * time.Millisecond,
		Render:      func(buf []float32) int { return len(buf) / 2 },
		OnInterrupt: func(err error) { interrupts = append(interrupts, err) },
	})
	if err != nil {
		t.Fatalf("NewAudioOutput: %v", err)
	}
	if len(factory.opened) != 1 {
		t.Fatalf("opened %d backends, want 1", len(factory.opened))
	}
	b := factory.opened[0]
	b.frames = 960
	b.at = time.Unix(10, 0)
	b.latency = 20 * time.Millisecond
	b.underruns = 3

	if err := out.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if f, at := out.Position(); f != 960 || !at.Equal(b.at) {
		t.Fatalf("Position = (%d, %v)", f, at)
	}
	if out.Latency() != 20*time.Millisecond || out.Underruns() != 3 || out.Flush() != 12 {
		t.Fatalf("metrics mismatch: latency=%v underruns=%d", out.Latency(), out.Underruns())
	}
	out.Pause()
	if b.started != 1 || b.paused != 1 || b.flushed != 1 {
		t.Fatalf("calls = started:%d paused:%d flushed:%d", b.started, b.paused, b.flushed)
	}

	// Interrupt callback posts onto the engine goroutine.
	wantErr := errors.New("device disconnected")
	b.conf.Interrupt(wantErr)
	if len(interrupts) != 0 {
		t.Fatal("interrupt ran synchronously before drain")
	}
	h.drain()
	if len(interrupts) != 1 || !errors.Is(interrupts[0], wantErr) {
		t.Fatalf("interrupts = %v, want [%v]", interrupts, wantErr)
	}

	// Scope disposal closes the backend and suppresses later interrupts.
	child.Dispose()
	if b.closed != 1 {
		t.Fatalf("backend closed %d times, want 1", b.closed)
	}
	b.conf.Interrupt(errors.New("late interrupt"))
	h.drain()
	if len(interrupts) != 1 {
		t.Fatalf("interrupt fired after close: %v", interrupts)
	}
	if err := out.Start(); err == nil {
		t.Fatal("Start succeeded after Close")
	}
}

func TestNewAudioOutputValidationAndAccess(t *testing.T) {
	factory := &fakeOutputFactory{}
	var cx *Scope
	h := newHost(t, Size{Width: 100, Height: 100}, nil)
	h.tree = NewTree(h.root, factory)
	h.tree.Resize(Size{Width: 100, Height: 100}, Geometry{})
	h.tree.Mount(func(c *Scope) Node { cx = c; return View{} })

	denied := cx.Child()
	denied.ProvideAccess(audioAccess{audio: false})
	if _, err := denied.NewAudioOutput(AudioOutputConfig{
		SampleRate: 48000, Channels: 2, Render: func([]float32) int { return 0 },
	}); !errors.Is(err, AudioNotAllowed) {
		t.Fatalf("denied access err = %v, want AudioNotAllowed", err)
	}

	for _, bad := range []AudioOutputConfig{
		{SampleRate: 0, Channels: 2, Render: func([]float32) int { return 0 }},
		{SampleRate: 48000, Channels: 0, Render: func([]float32) int { return 0 }},
		{SampleRate: 48000, Channels: 3, Render: func([]float32) int { return 0 }},
		{SampleRate: 48000, Channels: 2, Render: nil},
		{SampleRate: 48000, Channels: 2, Latency: -time.Millisecond, Render: func([]float32) int { return 0 }},
	} {
		if _, err := cx.NewAudioOutput(bad); !errors.Is(err, AudioNotSupported) {
			t.Fatalf("config %+v err = %v, want AudioNotSupported", bad, err)
		}
	}
}
