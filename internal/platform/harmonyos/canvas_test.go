//go:build js && wasm

package harmonyos

import (
	"encoding/json"
	"testing"

	"github.com/nsgo-dev/nsgo/internal/driver"
)

func meshVertex(x, y, u, v driver.Points, c driver.Color) driver.CanvasVertex {
	return driver.CanvasVertex{Position: driver.Point{X: x, Y: y}, TexCoord: driver.Point{X: u, Y: v}, Color: c}
}

// encode returns a drawing's image ops and mesh ops as the host reads them.
func encode(t *testing.T, d *driver.Drawing) (images, meshes []canvasOp) {
	t.Helper()
	var ops []canvasOp
	if err := json.Unmarshal([]byte(canvasJSON(d.Ops)), &ops); err != nil {
		t.Fatal(err)
	}
	for _, o := range ops {
		switch o.K {
		case int(driver.DrawImage):
			images = append(images, o)
		case int(driver.DrawVertices):
			meshes = append(meshes, o)
		}
	}
	return images, meshes
}

// TestCanvasEncodesMeshes checks that a mesh reaches the host as its vertices and mean alphas, and that two white triangles covering a crop reach it as the image op.
func TestCanvasEncodesMeshes(t *testing.T) {
	src := driver.ImageSource{URI: "asset:atlas.png"}
	at := meshVertex
	a, b, c, e := at(10, 10, 2, 2, 0), at(30, 10, 6, 2, 0), at(30, 30, 6, 6, 0), at(10, 30, 2, 6, 0)
	d := driver.NewDrawing(nil).
		DrawVertices(src, []driver.CanvasVertex{a, b, c, a, c, e}).
		SetImageSmoothing(false).
		DrawVertices(src, []driver.CanvasVertex{a, b, c, a, c, at(10, 30, 2, 6, driver.RGBA(0xffffff, 0))})
	images, meshes := encode(t, d)
	if len(images) != 1 || len(meshes) != 1 {
		t.Fatalf("%d images and %d meshes, want one each", len(images), len(meshes))
	}
	if im := images[0]; im.Img != src.URI || im.X != 10 || im.W != 20 || im.X1 != 2 || im.X2 != 4 || im.Y2 != 4 || im.N {
		t.Errorf("the quad's image op is %+v", im)
	}
	m := meshes[0]
	if m.Img != src.URI || len(m.V) != 24 || m.V[4] != 30 || m.V[6] != 6 || !m.N {
		t.Errorf("the mesh op is %+v", m)
	}
	if len(m.VA) != 2 || m.VA[0] != 1 || m.VA[1] != float32(2.0/3) {
		t.Errorf("the mesh's alphas are %v, want [1 2/3]", m.VA)
	}
	if m.E != nil {
		t.Errorf("the mesh's seams are %v, want none beside a translucent triangle", m.E)
	}
}

// TestCanvasEncodesNearestSampling checks that SetImageSmoothing(false) reaches the host on a plain image and on a mesh drawn as an image crop, as it does on other meshes.
func TestCanvasEncodesNearestSampling(t *testing.T) {
	src := driver.ImageSource{URI: "asset:atlas.png"}
	a, b, c, e := meshVertex(10, 10, 2, 2, 0), meshVertex(30, 10, 6, 2, 0), meshVertex(30, 30, 6, 6, 0), meshVertex(10, 30, 2, 6, 0)
	d := driver.NewDrawing(nil).
		DrawImage(src, 0, 0).
		SetImageSmoothing(false).
		DrawVertices(src, []driver.CanvasVertex{a, b, c, a, c, e}).
		DrawImageIn(src, 0, 0, 8, 8)
	images, meshes := encode(t, d)
	if len(images) != 3 || len(meshes) != 0 {
		t.Fatalf("%d images and %d meshes, want three images", len(images), len(meshes))
	}
	if images[0].N || !images[1].N || !images[2].N {
		t.Errorf("nearest sampling is %v, %v, %v, want false, true, true", images[0].N, images[1].N, images[2].N)
	}
}

// TestCanvasEncodesMeshSeams checks that the host learns which edges two opaque triangles share, and that a triangle whose texture coordinates meet at one texel still reaches it.
func TestCanvasEncodesMeshSeams(t *testing.T) {
	src := driver.ImageSource{URI: "asset:atlas.png"}
	at := meshVertex
	// A fan of three triangles around (0, 0), the last translucent, and one apart with a single texel's coordinates.
	o, p, q, r, s := at(0, 0, 1, 1, 0), at(10, 0, 5, 1, 0), at(10, 10, 5, 5, 0), at(0, 10, 1, 5, 0), at(-10, 5, 0, 3, driver.RGBA(0xffffff, 0.5))
	white := func(x, y driver.Points) driver.CanvasVertex { return at(x, y, 7, 7, 0) }
	d := driver.NewDrawing(nil).DrawVertices(src, []driver.CanvasVertex{
		o, p, q,
		q, r, o,
		r, s, o,
		white(40, 40), white(50, 40), white(40, 50),
	})
	_, meshes := encode(t, d)
	if len(meshes) != 1 {
		t.Fatalf("%d meshes, want one", len(meshes))
	}
	m := meshes[0]
	// Triangle 0 shares its edge 2 (q to o) with triangle 1's edge 2 (o to q); triangle 1's edge 1 (r to o) borders only the translucent triangle.
	if want := []int{0b100, 0b100, 0, 0}; len(m.E) != len(want) || m.E[0] != want[0] || m.E[1] != want[1] || m.E[2] != want[2] || m.E[3] != want[3] {
		t.Errorf("the seams are %v, want %v", m.E, want)
	}
	if len(m.V) != 48 || m.V[38] != 7 || m.V[47] != 7 {
		t.Errorf("the single-texel triangle is not sent whole: %v", m.V)
	}
}
