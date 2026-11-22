//go:build js && wasm

package harmonyos

import (
	"encoding/base64"
	"encoding/json"

	"github.com/nsgo-dev/nsgo/internal/driver"
	"github.com/nsgo-dev/nsgo/internal/platform/webstyle"
)

// canvasOp is one drawing operation as the host replays it into a
// CanvasRenderingContext2D. Short keys and omitted zero fields keep a
// drawing small.
type canvasOp struct {
	K    int       `json:"k"`
	X    float32   `json:"x,omitempty"`
	Y    float32   `json:"y,omitempty"`
	X1   float32   `json:"x1,omitempty"`
	Y1   float32   `json:"y1,omitempty"`
	X2   float32   `json:"x2,omitempty"`
	Y2   float32   `json:"y2,omitempty"`
	W    float32   `json:"w,omitempty"`
	H    float32   `json:"h,omitempty"`
	R    float32   `json:"r,omitempty"`
	A    float32   `json:"a,omitempty"`
	A0   float32   `json:"a0,omitempty"`
	A1   float32   `json:"a1,omitempty"`
	RY   float32   `json:"ry,omitempty"`
	CCW  bool      `json:"ccw,omitempty"`
	C    string    `json:"c,omitempty"`
	Cap  int       `json:"cap,omitempty"`
	Join int       `json:"join,omitempty"`
	EO   bool      `json:"eo,omitempty"`
	T    string    `json:"t,omitempty"`
	FF   string    `json:"ff,omitempty"`
	FS   float32   `json:"fs,omitempty"`
	FW   int       `json:"fw,omitempty"`
	FI   bool      `json:"fi,omitempty"`
	G    *gradient `json:"g,omitempty"`
	CO   int       `json:"co,omitempty"`
	D    []float32 `json:"d,omitempty"`
	Img  string    `json:"img,omitempty"`
	// ImgData is the image's bytes, base64, when the drawing embeds them.
	ImgData string `json:"imgd,omitempty"`
	// V is a mesh's vertices, x, y, u, v each, in canvas and image points;
	// VA its triangles' mean alpha, when a vertex color is not opaque white;
	// and N whether it samples the nearest texel.
	V  []float32 `json:"v,omitempty"`
	VA []float32 `json:"va,omitempty"`
	N  bool      `json:"n,omitempty"`
}

// gradient is a canvas gradient as the host builds one: radial or linear,
// its circles or endpoints, its stops.
type gradient struct {
	Radial bool    `json:"radial,omitempty"`
	X1     float32 `json:"x1,omitempty"`
	Y1     float32 `json:"y1,omitempty"`
	R1     float32 `json:"r1,omitempty"`
	X2     float32 `json:"x2,omitempty"`
	Y2     float32 `json:"y2,omitempty"`
	R2     float32 `json:"r2,omitempty"`
	Stops  []stop  `json:"s,omitempty"`
}

type stop struct {
	O float32 `json:"o"`
	C string  `json:"c"`
}

// canvasJSON serializes a drawing for the host.
func canvasJSON(ops []driver.DrawOp) string {
	out := make([]canvasOp, 0, len(ops))
	for i := range ops {
		o := &ops[i]
		c := canvasOp{
			K: int(o.Kind), X: o.X, Y: o.Y, X1: o.X1, Y1: o.Y1, X2: o.X2, Y2: o.Y2,
			W: o.W, H: o.H, R: o.R, A: o.A, A0: o.A0, A1: o.A1, RY: o.RY, CCW: o.CCW, EO: o.EvenOdd,
			T: o.Text, FF: o.Font.Family, FS: float32(o.Font.Size), FW: int(o.Font.Weight), FI: o.Font.Italic,
		}
		switch o.Kind {
		case driver.DrawFillColor, driver.DrawStrokeColor, driver.DrawShadow:
			c.C = webstyle.Color(o.Color)
		case driver.DrawLineCap:
			c.Cap = int(o.Cap) + 1
		case driver.DrawLineJoin:
			c.Join = int(o.Join) + 1
		case driver.DrawComposite:
			c.CO = int(o.Composite) + 1
		case driver.DrawFillGradient, driver.DrawStrokeGradient:
			c.G = gradientOf(o.Gradient)
		case driver.DrawLineDash:
			if o.Dash != nil {
				c.D = append([]float32(nil), (*o.Dash)...)
			}
		case driver.DrawVertices:
			if draw, ok := o.ImageRect(); ok {
				c = canvasOp{K: int(driver.DrawImage), X: draw.X, Y: draw.Y, W: draw.W, H: draw.H, X1: draw.X1, Y1: draw.Y1, X2: draw.X2, Y2: draw.Y2}
			} else if o.Mesh != nil {
				c = canvasOp{K: c.K, V: meshVertices(o.Mesh.Vertices), VA: meshAlphas(o.Mesh.Vertices), N: o.Sampling == driver.ImageSamplingNearest}
			}
			fallthrough
		case driver.DrawImage:
			if o.Image != nil {
				c.Img = o.Image.URI
				if len(o.Image.Data) > 0 {
					c.ImgData = base64.StdEncoding.EncodeToString(o.Image.Data)
				}
			}
		}
		out = append(out, c)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// meshVertices flattens a mesh's positions and texture coordinates.
func meshVertices(vertices []driver.CanvasVertex) []float32 {
	out := make([]float32, 0, len(vertices)*4)
	for _, v := range vertices {
		out = append(out, float32(v.Position.X), float32(v.Position.Y), float32(v.TexCoord.X), float32(v.TexCoord.Y))
	}
	return out
}

// meshAlphas is each triangle's mean vertex alpha, or nil when every vertex is opaque, which the host plays as the triangle's alpha: an ArkUI canvas has no way to multiply an image by interpolated colors.
func meshAlphas(vertices []driver.CanvasVertex) []float32 {
	var out []float32
	for i := 0; i+2 < len(vertices); i += 3 {
		var sum float64
		for _, v := range vertices[i : i+3] {
			a := 1.0
			if v.Color != 0 {
				_, _, _, a = v.Color.Components()
			}
			sum += a
		}
		if out == nil && sum < 3 {
			out = make([]float32, i/3, len(vertices)/3)
			for j := range out {
				out[j] = 1
			}
		}
		if out != nil {
			out = append(out, float32(sum/3))
		}
	}
	return out
}

// gradientOf maps a drawing gradient, leaving out what the host cannot play.
func gradientOf(g *driver.CanvasGradient) *gradient {
	if g == nil || g.Conic {
		return nil
	}
	out := &gradient{Radial: g.Radial, X1: g.X1, Y1: g.Y1, R1: g.R1, X2: g.X2, Y2: g.Y2, R2: g.R2}
	for _, s := range g.Stops {
		out.Stops = append(out.Stops, stop{O: s.Offset, C: webstyle.Color(s.Color)})
	}
	return out
}
