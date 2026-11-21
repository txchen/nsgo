package android

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/nsgo-dev/nsgo/internal/driver"
	"github.com/nsgo-dev/nsgo/internal/testhost"
)

func argbOf(c driver.Color) uint32 {
	v := uint32(c)
	return (v >> 8) | (v << 24)
}

// TestEdgeEffectConstantsMatchJava holds Protocol.java's EDGE_EFFECT_*
// constants to driver.EdgeEffect, which writeScrollEdges encodes and
// BlurGeometry/ScrollEdges switch on.
func TestEdgeEffectConstantsMatchJava(t *testing.T) {
	testhost.RequireSource(t)
	java, err := os.ReadFile(filepath.Join("java", "dev", "oneo", "nsgo", "Protocol.java"))
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]int{}
	for _, m := range regexp.MustCompile(`static final int (EDGE_EFFECT_\w+) = (\d+);`).FindAllStringSubmatch(string(java), -1) {
		values[m[1]], _ = strconv.Atoi(m[2])
	}
	for name, want := range map[string]driver.EdgeEffect{
		"EDGE_EFFECT_AUTOMATIC": driver.EdgeEffectAutomatic,
		"EDGE_EFFECT_SOFT":      driver.EdgeEffectSoft,
		"EDGE_EFFECT_HARD":      driver.EdgeEffectHard,
		"EDGE_EFFECT_HIDDEN":    driver.EdgeEffectHidden,
	} {
		if got, ok := values[name]; !ok || got != int(want) {
			t.Errorf("Protocol.java's %s is %d (found %v), driver's %d", name, got, ok, want)
		}
	}
}

// TestBackdropFiltersEncode checks that writeBackdropFilters writes the
// combined color matrix and widest blur radius, omitting drop shadows.
func TestBackdropFiltersEncode(t *testing.T) {
	// Empty filters: no color matrix, 0 blur.
	var w writer
	writeBackdropFilters(&w, nil)
	r := reader{w.buf}
	if hasColor, blur := r.byte(), r.points(); hasColor != 0 || blur != 0 || len(r.buf) != 0 {
		t.Fatalf("empty: color=%d blur=%v left=%d", hasColor, blur, len(r.buf))
	}

	// Multiple blurs + color filters + drop shadow (ignored).
	filters := []driver.Filter{
		{Kind: driver.FilterBlur, Amount: 8},
		{Kind: driver.FilterSaturate, Amount: 1.8},
		{Kind: driver.FilterBrightness, Amount: 1.1},
		{Kind: driver.FilterContrast, Amount: 0.9},
		{Kind: driver.FilterBlur, Amount: 16},
		{Kind: driver.FilterDropShadow, Shadow: driver.BoxShadow{Blur: 10, Color: driver.RGB(0)}},
	}
	wantM, wantColor, wantBlur, _ := driver.SplitFilters(filters)
	w = writer{}
	writeBackdropFilters(&w, filters)
	r = reader{w.buf}
	if hasColor := r.byte(); (hasColor != 0) != wantColor {
		t.Fatalf("hasColor = %d, want %v", hasColor, wantColor)
	}
	for i := range 20 {
		if got := r.float(); got != wantM[i] {
			t.Fatalf("matrix[%d] = %v, want %v", i, got, wantM[i])
		}
	}
	if gotBlur := r.points(); gotBlur != wantBlur || len(r.buf) != 0 {
		t.Fatalf("blur = %v (want %v), %d bytes left", gotBlur, wantBlur, len(r.buf))
	}
}

// TestScrollEdgesEncode checks that writeScrollEdges resolves automatic edges
// via Drawn() and encodes only active sides' geometry and colors.
func TestScrollEdgesEncode(t *testing.T) {
	// All inactive (automatic with 0 extent, explicit hidden with extent).
	var w writer
	writeScrollEdges(&w, [4]driver.ScrollEdge{
		{Effect: driver.EdgeEffectAutomatic, Extent: 0},
		{Effect: driver.EdgeEffectHidden, Extent: 48, Tint: 0xffffffff},
		{Effect: driver.EdgeEffectSoft, Extent: 0},
		{Effect: driver.EdgeEffectHard, Extent: 0},
	})
	if len(w.buf) != 4 {
		t.Fatalf("inactive edges encoded %d bytes, want 4", len(w.buf))
	}
	for i, b := range w.buf {
		if b != byte(driver.EdgeEffectHidden) {
			t.Errorf("side %d = %d, want hidden (%d)", i, b, driver.EdgeEffectHidden)
		}
	}

	// Mixed active sides: automatic+extent -> soft, hard, soft, hidden.
	edges := [4]driver.ScrollEdge{
		{Effect: driver.EdgeEffectAutomatic, Extent: 64, Tint: 0xf0f4f8ff, Line: 0x00000022},
		{Effect: driver.EdgeEffectHard, Extent: 32, Tint: 0x112233ff, Line: 0x445566cc},
		{Effect: driver.EdgeEffectSoft, Extent: 16, Tint: 0xffffffff},
		{Effect: driver.EdgeEffectHidden, Extent: 50, Tint: 0xffffffff},
	}
	w = writer{}
	writeScrollEdges(&w, edges)
	r := reader{w.buf}

	// Side 0: automatic with extent 64 -> soft.
	if eff, ext, tint, line := driver.EdgeEffect(r.byte()), r.points(), driver.Color(r.u32()), driver.Color(r.u32()); eff != driver.EdgeEffectSoft || ext != 64 || tint != 0xf0f4f8ff || line != 0x00000022 {
		t.Errorf("side 0 = (%v, %v, %#x, %#x)", eff, ext, tint, line)
	}
	// Side 1: hard.
	if eff, ext, tint, line := driver.EdgeEffect(r.byte()), r.points(), driver.Color(r.u32()), driver.Color(r.u32()); eff != driver.EdgeEffectHard || ext != 32 || tint != 0x112233ff || line != 0x445566cc {
		t.Errorf("side 1 = (%v, %v, %#x, %#x)", eff, ext, tint, line)
	}
	// Side 2: soft.
	if eff, ext, tint, line := driver.EdgeEffect(r.byte()), r.points(), driver.Color(r.u32()), driver.Color(r.u32()); eff != driver.EdgeEffectSoft || ext != 16 || tint != 0xffffffff || line != 0 {
		t.Errorf("side 2 = (%v, %v, %#x, %#x)", eff, ext, tint, line)
	}
	// Side 3: hidden (1 byte only).
	if eff := driver.EdgeEffect(r.byte()); eff != driver.EdgeEffectHidden || len(r.buf) != 0 {
		t.Errorf("side 3 = %v, %d bytes left", eff, len(r.buf))
	}
}

// TestBlurGeometryMatchesJava compiles BlurGeometry.java and Protocol.java and
// runs BlurGeometryCheck on the JVM with Go-encoded backdrop and scroll edge
// payloads, verifying exact wire parity and math agreement with internal/driver.
func TestBlurGeometryMatchesJava(t *testing.T) {
	testhost.RequireSource(t)
	javac, err := exec.LookPath("javac")
	if err != nil || exec.Command(javac, "-version").Run() != nil {
		t.Skip("no JDK")
	}

	type backdropCase struct {
		density float32
		filters []driver.Filter
	}
	backdrops := []backdropCase{
		{2.0, nil},
		{2.5, []driver.Filter{{Kind: driver.FilterBlur, Amount: 12}}},
		{3.0, []driver.Filter{{Kind: driver.FilterSaturate, Amount: 1.5}, {Kind: driver.FilterContrast, Amount: 1.2}}},
		{2.0, []driver.Filter{
			{Kind: driver.FilterBlur, Amount: 10},
			{Kind: driver.FilterBrightness, Amount: 0.9},
			{Kind: driver.FilterInvert, Amount: 0.25},
			{Kind: driver.FilterHueRotate, Amount: 45},
			{Kind: driver.FilterGrayscale, Amount: 0.3},
			{Kind: driver.FilterSepia, Amount: 0.2},
			{Kind: driver.FilterOpacity, Amount: 0.85},
		}},
	}

	type edgeCase struct {
		w, h, density float32
		edges         [4]driver.ScrollEdge
	}
	edgeCases := []edgeCase{
		{
			w: 1080, h: 2400, density: 3.0,
			edges: [4]driver.ScrollEdge{
				{Effect: driver.EdgeEffectAutomatic, Extent: 64, Tint: 0xf8fafcff, Line: 0x00000033},
				{Effect: driver.EdgeEffectHard, Extent: 40, Tint: 0x101820ff, Line: 0xffffff40},
				{Effect: driver.EdgeEffectSoft, Extent: 12, Tint: 0xffffffff},
				{Effect: driver.EdgeEffectHidden, Extent: 30, Tint: 0xffffffff},
			},
		},
		{
			w: 400, h: 300, density: 2.0,
			edges: [4]driver.ScrollEdge{
				{Effect: driver.EdgeEffectHard, Extent: 56, Tint: 0x202020ee, Line: 0x808080ff},
				{Effect: driver.EdgeEffectAutomatic, Extent: 0},
				{Effect: driver.EdgeEffectHard, Extent: 48, Tint: 0x000000cc, Line: 0xffffffff},
				{Effect: driver.EdgeEffectSoft, Extent: 220, Tint: 0xabcdef80},
			},
		},
	}

	type padCase struct {
		l, top, r, b, blur float32
		cl, ctop, cr, cb   float32
		wantNil            bool
		wl, wt, wr, wb     int
	}
	padCases := []padCase{
		// Interior box expanded by 3*blur (60px).
		{100, 200, 300, 260, 20, 0, 0, 1080, 2400, false, 40, 140, 360, 320},
		// Top-edge bar clamped at top/left/right, padded on bottom.
		{0, 0, 1080, 80, 20, 0, 0, 1080, 2400, false, 0, 0, 1080, 140},
		// Zero blur keeps exact bounds.
		{10.5, 20.2, 100.1, 80.9, 0, 0, 0, 500, 500, false, 10, 20, 101, 81},
		// Completely outside surface -> null.
		{600, 600, 700, 700, 10, 0, 0, 500, 500, true, 0, 0, 0, 0},
	}

	w := &writer{}
	w.u32(uint32(len(backdrops)))
	for _, bc := range backdrops {
		w.float(bc.density)
		writeBackdropFilters(w, bc.filters)
	}
	w.u32(uint32(len(edgeCases)))
	for _, ec := range edgeCases {
		w.floats(ec.w, ec.h, ec.density)
		writeScrollEdges(w, ec.edges)
	}
	w.u32(uint32(len(padCases)))
	for _, pc := range padCases {
		w.floats(pc.l, pc.top, pc.r, pc.b, pc.blur, pc.cl, pc.ctop, pc.cr, pc.cb)
	}

	dir := t.TempDir()
	input := filepath.Join(dir, "blur.bin")
	if err := os.WriteFile(input, w.buf, 0o644); err != nil {
		t.Fatal(err)
	}
	build := exec.CommandContext(t.Context(), javac, "-d", dir,
		"java/dev/oneo/nsgo/Protocol.java",
		"java/dev/oneo/nsgo/BlurGeometry.java",
		"testdata/BlurGeometryCheck.java")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("javac: %v\n%s", err, out)
	}
	javaBin := filepath.Join(filepath.Dir(javac), "java")
	out, err := exec.CommandContext(t.Context(), javaBin, "-cp", dir, "dev.oneo.nsgo.BlurGeometryCheck", input).CombinedOutput()
	if err != nil {
		t.Fatalf("java: %v\n%s", err, out)
	}

	// Build expected lines from internal/driver.
	var want []string
	for i, bc := range backdrops {
		m, color, blur, _ := driver.SplitFilters(bc.filters)
		active := blur > 0 || color
		line := fmt.Sprintf("backdrop %d %v %v %v", i, active, float32(blur)*bc.density, color)
		if color {
			for k := range 20 {
				v := m[k]
				if k%5 == 4 {
					v *= 255
				}
				line += fmt.Sprintf(" %v", v)
			}
		}
		want = append(want, line)
	}
	for i, ec := range edgeCases {
		anyActive := false
		for side := range 4 {
			if ec.edges[side].Drawn() != driver.EdgeEffectHidden {
				anyActive = true
			}
		}
		want = append(want, fmt.Sprintf("edges %d %v true", i, anyActive))
		for side := range 4 {
			e := ec.edges[side]
			drawn := e.Drawn()
			active := drawn != driver.EdgeEffectHidden
			var ext, band driver.Points
			var tint, lineCol uint32
			if active {
				ext = e.Extent
				band = e.Band()
				tint = argbOf(e.Tint)
				lineCol = argbOf(e.Line)
			}
			want = append(want, fmt.Sprintf("edge %d %d %d %v %v %v %08x %08x",
				i, side, drawn, active, float32(ext), float32(band), tint, lineCol))
			if !active {
				continue
			}
			br := driver.BandRect(driver.Side(side), driver.Size{Width: driver.Points(ec.w), Height: driver.Points(ec.h)}, driver.Points(float32(band)*ec.density))
			x0, y0, x1, y1 := driver.EdgeLine(driver.Side(side))
			want = append(want, fmt.Sprintf("band %d %d %v %v %v %v %v %v %v %v",
				i, side, float32(br.Origin.X), float32(br.Origin.Y), float32(br.Size.Width), float32(br.Size.Height), x0, y0, x1, y1))
			if drawn == driver.EdgeEffectHard {
				hr := driver.HairlineRect(driver.Side(side), br.Size)
				hw := float32(driver.HairlineWidth) * ec.density
				// Scale HairlineRect thickness from points to pixels.
				hx, hy, hwOut, hhOut := float32(br.Origin.X), float32(br.Origin.Y), float32(hr.Size.Width), float32(hr.Size.Height)
				switch driver.Side(side) {
				case driver.SideTop:
					hy += float32(br.Size.Height)
					hhOut = hw
				case driver.SideRight:
					hx -= hw
					hwOut = hw
				case driver.SideBottom:
					hy -= hw
					hhOut = hw
				case driver.SideLeft:
					hx += float32(br.Size.Width)
					hwOut = hw
				}
				want = append(want, fmt.Sprintf("hairline %d %d %v %v %v %v %08x %08x",
					i, side, hx, hy, hwOut, hhOut,
					argbOf(e.Tint.Faded(driver.EdgeTint)),
					argbOf(e.Tint.Faded(driver.UnblurredHardTint))))
			} else {
				for _, most := range []float32{driver.EdgeTint, driver.UnblurredSoftTint} {
					stops := e.SoftTint(most)
					var sb strings.Builder
					fmt.Fprintf(&sb, "softtint %d %d %v", i, side, most)
					for _, st := range stops {
						fmt.Fprintf(&sb, " %v %08x", st.Offset, argbOf(st.Color))
					}
					want = append(want, sb.String())
				}
			}
		}
	}
	levels := driver.SoftEdgeLevels()
	want = append(want, fmt.Sprintf("levels %d", len(levels)))
	for k, lv := range levels {
		want = append(want, fmt.Sprintf("level %d %v %v %v %v", k, float32(lv.Blur), float32(lv.Total), lv.Full, lv.Gone))
	}
	for i, pc := range padCases {
		if pc.wantNil {
			want = append(want, fmt.Sprintf("pad %d null", i))
		} else {
			want = append(want, fmt.Sprintf("pad %d %d %d %d %d", i, pc.wl, pc.wt, pc.wr, pc.wb))
		}
	}

	got := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(got) != len(want) {
		t.Fatalf("Java printed %d lines, want %d\ngot:\n%s\nwant:\n%s", len(got), len(want), strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for i := range want {
		if !sameBlurLine(got[i], want[i]) {
			t.Errorf("line %d differs:\n java: %s\n core: %s", i, got[i], want[i])
		}
	}
}

func sameBlurLine(a, b string) bool {
	x, y := strings.Fields(a), strings.Fields(b)
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] == y[i] {
			continue
		}
		p, err1 := strconv.ParseFloat(x[i], 64)
		q, err2 := strconv.ParseFloat(y[i], 64)
		if err1 != nil || err2 != nil {
			return false
		}
		if math.Abs(p-q) > max(1e-4, 1e-4*math.Abs(q)) {
			return false
		}
	}
	return true
}
