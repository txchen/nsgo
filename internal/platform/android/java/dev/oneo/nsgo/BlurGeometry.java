package dev.oneo.nsgo;

import java.io.DataInputStream;
import java.io.IOException;

/**
 * Pure-Java protocol decoding, color math and geometry shared by Backdrop
 * (view backdrop filters) and ScrollEdges (scroll view soft and hard edge
 * effects). Kept free of Android SDK imports so JVM tests verify parity with
 * internal/driver and internal/platform/android/protocol.go directly.
 */
final class BlurGeometry {
    /** Matches internal/driver/scroll.go constants, lengths in points. */
    static final float SOFT_BLUR = 10f;
    static final float SOFT_TAIL = 24f;
    static final float HARD_BLUR = 6.4f;
    static final float EDGE_TINT = 0.8f;
    static final float HAIRLINE_WIDTH = 0.5f;
    static final float UNBLURRED_SOFT_TINT = 0.85f;
    static final float UNBLURRED_HARD_TINT = 0.95f;
    static final float FINEST_LEVEL = 1.5f;

    /** Side indices matching driver.Side (top, right, bottom, left). */
    static final int SIDE_TOP = 0;
    static final int SIDE_RIGHT = 1;
    static final int SIDE_BOTTOM = 2;
    static final int SIDE_LEFT = 3;

    private BlurGeometry() {}

    /** Decoded backdrop filter payload: color matrix (0..1 offsets, or null) and blur radius in pixels. */
    static final class BackdropSpec {
        final float[] colorMatrix;
        final float blur;

        BackdropSpec(float[] colorMatrix, float blur) {
            this.colorMatrix = colorMatrix;
            this.blur = blur;
        }

        boolean active() {
            return blur > 0 || colorMatrix != null;
        }
    }

    /** Reads what protocol.go's writeBackdropFilters writes, scaling blur points by density. */
    static BackdropSpec readBackdrop(DataInputStream in, float density) throws IOException {
        float[] matrix = null;
        if (Protocol.flag(in.readUnsignedByte(), 0)) {
            matrix = new float[20];
            for (int i = 0; i < 20; i++) matrix[i] = in.readFloat();
        }
        float blur = Math.max(0f, in.readFloat() * density);
        return new BackdropSpec(matrix, blur);
    }

    /**
     * Converts a 4×5 driver.ColorMatrix (offsets in 0..1) into Android's
     * ColorMatrix array (offsets in 0..255).
     */
    static float[] colorMatrix255(float[] matrix) {
        float[] m = matrix.clone();
        for (int i = 4; i < 20; i += 5) m[i] *= 255f;
        return m;
    }

    /**
     * Expands a target rectangle [left, top, right, bottom] by 3*blurPx on each
     * side so Gaussian blur samples real surrounding pixels, clamped to the
     * available surface rectangle [clampLeft, clampTop, clampRight, clampBottom].
     * Returns {srcLeft, srcTop, srcRight, srcBottom} or null if empty.
     */
    static int[] paddedBounds(float left, float top, float right, float bottom, float blurPx,
            float clampLeft, float clampTop, float clampRight, float clampBottom) {
        int pad = blurPx > 0f && Float.isFinite(blurPx) ? (int) Math.ceil(blurPx * 3f) : 0;
        int cLeft = (int) Math.floor(clampLeft);
        int cTop = (int) Math.floor(clampTop);
        int cRight = (int) Math.ceil(clampRight);
        int cBottom = (int) Math.ceil(clampBottom);
        int srcLeft = Math.max(cLeft, (int) Math.floor(left - pad));
        int srcTop = Math.max(cTop, (int) Math.floor(top - pad));
        int srcRight = Math.min(cRight, (int) Math.ceil(right + pad));
        int srcBottom = Math.min(cBottom, (int) Math.ceil(bottom + pad));
        if (srcRight <= srcLeft || srcBottom <= srcTop) return null;
        return new int[] {srcLeft, srcTop, srcRight, srcBottom};
    }

    /** One side's scroll edge effect (driver.ScrollEdge), extent in points and colors in ARGB. */
    static final class Edge {
        int effect = Protocol.EDGE_EFFECT_HIDDEN;
        float extent;
        int tint;
        int line;

        boolean active() {
            return (effect == Protocol.EDGE_EFFECT_SOFT || effect == Protocol.EDGE_EFFECT_HARD) && extent > 0f;
        }

        /** How far from the side the drawn effect reaches, in points (driver.ScrollEdge.Band). */
        float band() {
            if (effect == Protocol.EDGE_EFFECT_SOFT && extent > 0f) return extent + SOFT_TAIL;
            if (effect == Protocol.EDGE_EFFECT_HARD && extent > 0f) return extent;
            return 0f;
        }

        boolean sameAs(Edge o) {
            if (o == null) return false;
            return effect == o.effect && extent == o.extent && tint == o.tint && line == o.line;
        }
    }

    /** Reads what protocol.go's writeScrollEdges writes for all four sides (top, right, bottom, left). */
    static Edge[] readEdges(DataInputStream in) throws IOException {
        Edge[] edges = new Edge[4];
        for (int i = 0; i < 4; i++) {
            Edge e = new Edge();
            e.effect = in.readUnsignedByte();
            if (e.effect != Protocol.EDGE_EFFECT_HIDDEN) {
                e.extent = in.readFloat();
                e.tint = Protocol.argb(in.readInt());
                e.line = Protocol.argb(in.readInt());
            }
            edges[i] = e;
        }
        return edges;
    }

    /** Whether any of the four sides has an active edge effect. */
    static boolean anyEdge(Edge[] edges) {
        if (edges == null) return false;
        for (Edge e : edges) {
            if (e != null && e.active()) return true;
        }
        return false;
    }

    /** Whether two four-side edge arrays draw identically. */
    static boolean edgesEqual(Edge[] a, Edge[] b) {
        if (a == b) return true;
        if (a == null || b == null || a.length != b.length) return false;
        for (int i = 0; i < a.length; i++) {
            if (a[i] == null ? b[i] != null : !a[i].sameAs(b[i])) return false;
        }
        return true;
    }

    /** One progressive blur level of a soft edge, matching driver.BlurLevel. */
    static final class Level {
        final float blur;
        final float total;
        final float full;
        final float gone;

        Level(float blur, float total, float full, float gone) {
            this.blur = blur;
            this.total = total;
            this.full = full;
            this.gone = gone;
        }
    }

    /** Returns the progressive blur levels of a soft edge, matching driver.SoftEdgeLevels. */
    static Level[] softLevels() {
        float most = SOFT_BLUR;
        float[] his = new float[8];
        int n = 0;
        his[n++] = most;
        for (float h = most; h > FINEST_LEVEL; ) {
            h /= 2f;
            his[n++] = h;
        }
        Level[] levels = new Level[n];
        float lo = 0f;
        for (int i = n - 1, out = 0; i >= 0; i--, out++) {
            float hi = his[i];
            float blur = (float) Math.sqrt((double) hi * hi - (double) lo * lo);
            levels[out] = new Level(blur, hi, 1f - hi / most, 1f - lo / most);
            lo = hi;
        }
        return levels;
    }

    /** Gradient stops for a soft edge's tint (driver.ScrollEdge.SoftTint). */
    static final class TintStops {
        final int[] colors;
        final float[] offsets;

        TintStops(int[] colors, float[] offsets) {
            this.colors = colors;
            this.offsets = offsets;
        }
    }

    /**
     * Smoothstep tint stops across a soft edge's band, from the side to the
     * band's end: flat at most until SOFT_TAIL short of the bars' end, then
     * easing out to transparent (driver.ScrollEdge.SoftTint).
     */
    static TintStops softTint(float extent, int tintArgb, float most) {
        float band = extent + SOFT_TAIL;
        if (extent <= 0f || band <= 0f) return null;
        float from = Math.max(extent - SOFT_TAIL, 0f) / band;
        int steps = 8;
        int[] colors = new int[steps + 1];
        float[] offsets = new float[steps + 1];
        for (int i = 0; i <= steps; i++) {
            float t = (float) i / steps;
            float smooth = 1f - t * t * (3f - 2f * t);
            colors[i] = fadeColor(tintArgb, most * smooth);
            offsets[i] = from + (1f - from) * t;
        }
        return new TintStops(colors, offsets);
    }

    /** Multiplies argb's alpha channel by factor clamped to [0, 1], matching driver.Color.Faded. */
    static int fadeColor(int argb, float factor) {
        float f = Math.max(0f, Math.min(1f, factor));
        int a = (int) (((argb >>> 24) & 0xff) * f + 0.5f);
        return (argb & 0x00ffffff) | (a << 24);
    }

    /** Returns {x, y, w, h} for side's band in a viewport of width × height (driver.BandRect). */
    static float[] bandRect(int side, float width, float height, float band) {
        switch (side) {
            case SIDE_RIGHT: {
                float b = Math.min(band, width);
                return new float[] {width - b, 0f, b, height};
            }
            case SIDE_BOTTOM: {
                float b = Math.min(band, height);
                return new float[] {0f, height - b, width, b};
            }
            case SIDE_LEFT:
                return new float[] {0f, 0f, Math.min(band, width), height};
            default: // SIDE_TOP
                return new float[] {0f, 0f, width, Math.min(band, height)};
        }
    }

    /** Returns {x0, y0, x1, y1} gradient direction across a band from side (driver.EdgeLine). */
    static float[] edgeLine(int side) {
        switch (side) {
            case SIDE_RIGHT:
                return new float[] {1f, 0.5f, 0f, 0.5f};
            case SIDE_BOTTOM:
                return new float[] {0.5f, 1f, 0.5f, 0f};
            case SIDE_LEFT:
                return new float[] {0f, 0.5f, 1f, 0.5f};
            default: // SIDE_TOP
                return new float[] {0.5f, 0f, 0.5f, 1f};
        }
    }

    /**
     * Returns {x, y, w, h} for a hard edge's hairline just outside its band
     * {bx, by, bw, bh}, hw thick, in the scroll view's coordinates
     * (driver.HairlineRect translated by band origin).
     */
    static float[] hairlineRect(int side, float bx, float by, float bw, float bh, float hw) {
        switch (side) {
            case SIDE_RIGHT:
                return new float[] {bx - hw, by, hw, bh};
            case SIDE_BOTTOM:
                return new float[] {bx, by - hw, bw, hw};
            case SIDE_LEFT:
                return new float[] {bx + bw, by, hw, bh};
            default: // SIDE_TOP
                return new float[] {bx, by + bh, bw, hw};
        }
    }
}
