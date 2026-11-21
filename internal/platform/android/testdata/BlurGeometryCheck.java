package dev.oneo.nsgo;

import java.io.DataInputStream;
import java.io.FileInputStream;
import java.util.Locale;

/**
 * JVM check driven by internal/platform/android/blur_test.go: reads Go-encoded
 * backdrop filter and scroll edge payloads via BlurGeometry, prints the decoded
 * results and derived geometry/color tables so Go compares them against
 * internal/driver.
 */
public final class BlurGeometryCheck {
    public static void main(String[] args) throws Exception {
        Locale.setDefault(Locale.ROOT);
        try (DataInputStream in = new DataInputStream(new FileInputStream(args[0]))) {
            // 1. Backdrop specs.
            int backdropCount = in.readInt();
            for (int i = 0; i < backdropCount; i++) {
                float density = in.readFloat();
                BlurGeometry.BackdropSpec spec = BlurGeometry.readBackdrop(in, density);
                StringBuilder sb = new StringBuilder();
                sb.append("backdrop ").append(i)
                        .append(' ').append(spec.active())
                        .append(' ').append(spec.blur)
                        .append(' ').append(spec.colorMatrix != null);
                if (spec.colorMatrix != null) {
                    float[] m255 = BlurGeometry.colorMatrix255(spec.colorMatrix);
                    for (float v : m255) sb.append(' ').append(v);
                }
                System.out.println(sb);
            }

            // 2. Scroll edges + band/line/hairline/softTint geometry.
            int edgeCases = in.readInt();
            for (int i = 0; i < edgeCases; i++) {
                float width = in.readFloat();
                float height = in.readFloat();
                float density = in.readFloat();
                BlurGeometry.Edge[] edges = BlurGeometry.readEdges(in);
                System.out.printf("edges %d %b %b%n", i, BlurGeometry.anyEdge(edges), BlurGeometry.edgesEqual(edges, edges));
                for (int side = 0; side < 4; side++) {
                    BlurGeometry.Edge e = edges[side];
                    System.out.printf("edge %d %d %d %b %s %s %08x %08x%n",
                            i, side, e.effect, e.active(), e.extent, e.band(), e.tint, e.line);
                    if (!e.active()) continue;
                    float[] br = BlurGeometry.bandRect(side, width, height, e.band() * density);
                    float[] el = BlurGeometry.edgeLine(side);
                    System.out.printf("band %d %d %s %s %s %s %s %s %s %s%n",
                            i, side, br[0], br[1], br[2], br[3], el[0], el[1], el[2], el[3]);
                    if (e.effect == Protocol.EDGE_EFFECT_HARD) {
                        float[] hr = BlurGeometry.hairlineRect(side, br[0], br[1], br[2], br[3],
                                BlurGeometry.HAIRLINE_WIDTH * density);
                        System.out.printf("hairline %d %d %s %s %s %s %08x %08x%n",
                                i, side, hr[0], hr[1], hr[2], hr[3],
                                BlurGeometry.fadeColor(e.tint, BlurGeometry.EDGE_TINT),
                                BlurGeometry.fadeColor(e.tint, BlurGeometry.UNBLURRED_HARD_TINT));
                    } else {
                        for (float most : new float[] {BlurGeometry.EDGE_TINT, BlurGeometry.UNBLURRED_SOFT_TINT}) {
                            BlurGeometry.TintStops ts = BlurGeometry.softTint(e.extent, e.tint, most);
                            StringBuilder sb = new StringBuilder();
                            sb.append("softtint ").append(i).append(' ').append(side).append(' ').append(most);
                            for (int k = 0; k < ts.colors.length; k++) {
                                sb.append(' ').append(ts.offsets[k])
                                        .append(' ').append(String.format("%08x", ts.colors[k]));
                            }
                            System.out.println(sb);
                        }
                    }
                }
            }

            // 3. Soft edge progressive blur levels.
            BlurGeometry.Level[] levels = BlurGeometry.softLevels();
            System.out.println("levels " + levels.length);
            for (int k = 0; k < levels.length; k++) {
                BlurGeometry.Level lv = levels[k];
                System.out.printf("level %d %s %s %s %s%n", k, lv.blur, lv.total, lv.full, lv.gone);
            }

            // 4. Padded blur bounds clamping.
            int padCases = in.readInt();
            for (int i = 0; i < padCases; i++) {
                int[] r = BlurGeometry.paddedBounds(
                        in.readFloat(), in.readFloat(), in.readFloat(), in.readFloat(),
                        in.readFloat(),
                        in.readFloat(), in.readFloat(), in.readFloat(), in.readFloat());
                if (r == null) {
                    System.out.printf("pad %d null%n", i);
                } else {
                    System.out.printf("pad %d %d %d %d %d%n", i, r[0], r[1], r[2], r[3]);
                }
            }
        }
    }
}
