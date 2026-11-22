//go:build js && wasm

package harmonyos

import (
	"encoding/json"
	"testing"

	"github.com/nsgo-dev/nsgo/internal/driver"
)

// TestCanvasEncodesMeshes checks that a mesh reaches the host as its vertices and mean alphas, and that two white triangles covering a crop reach it as the image op.
func TestCanvasEncodesMeshes(t *testing.T) {
	src := driver.ImageSource{URI: "asset:atlas.png"}
	at := func(x, y, u, v driver.Points, c driver.Color) driver.CanvasVertex {
		return driver.CanvasVertex{Position: driver.Point{X: x, Y: y}, TexCoord: driver.Point{X: u, Y: v}, Color: c}
	}
	a, b, c, e := at(10, 10, 2, 2, 0), at(30, 10, 6, 2, 0), at(30, 30, 6, 6, 0), at(10, 30, 2, 6, 0)
	d := driver.NewDrawing(nil).
		DrawVertices(src, []driver.CanvasVertex{a, b, c, a, c, e}).
		SetImageSmoothing(false).
		DrawVertices(src, []driver.CanvasVertex{a, b, c, a, c, at(10, 30, 2, 6, driver.RGBA(0xffffff, 0))})
	var ops []canvasOp
	if err := json.Unmarshal([]byte(canvasJSON(d.Ops)), &ops); err != nil {
		t.Fatal(err)
	}
	var images, meshes []canvasOp
	for _, o := range ops {
		switch o.K {
		case int(driver.DrawImage):
			images = append(images, o)
		case int(driver.DrawVertices):
			meshes = append(meshes, o)
		}
	}
	if len(images) != 1 || len(meshes) != 1 {
		t.Fatalf("%d images and %d meshes, want one each", len(images), len(meshes))
	}
	if im := images[0]; im.Img != src.URI || im.X != 10 || im.W != 20 || im.X1 != 2 || im.X2 != 4 || im.Y2 != 4 {
		t.Errorf("the quad's image op is %+v", im)
	}
	m := meshes[0]
	if m.Img != src.URI || len(m.V) != 24 || m.V[4] != 30 || m.V[6] != 6 || !m.N {
		t.Errorf("the mesh op is %+v", m)
	}
	if len(m.VA) != 2 || m.VA[0] != 1 || m.VA[1] != float32(2.0/3) {
		t.Errorf("the mesh's alphas are %v, want [1 2/3]", m.VA)
	}
}
