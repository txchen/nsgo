package dev.oneo.nsgo;

import android.graphics.BlendMode;
import android.graphics.Canvas;
import android.graphics.LinearGradient;
import android.graphics.Paint;
import android.graphics.RecordingCanvas;
import android.graphics.Rect;
import android.graphics.RenderEffect;
import android.graphics.RenderNode;
import android.graphics.Shader;
import android.graphics.drawable.Drawable;
import android.os.Build;
import android.view.View;
import android.view.ViewGroup;

/**
 * Draws a scroll view's soft and hard edge effects over its scrolling content,
 * matching docs/architecture/scrolling.md#edge-effects and internal/driver/scroll.go:
 *
 * - Android 12+ (API 31) with hardware acceleration: records the scroll view's
 *   background and scroller into a RenderNode, then composites blurred bands
 *   via RenderEffect (four progressive blur levels masked by linear gradients
 *   for soft edges; an even blur clipped to the band for hard edges), followed
 *   by the 80% tint (smoothstep gradient for soft, flat for hard) and a 0.5pt
 *   hairline for hard edges.
 * - API < 31 (or software canvas): falls back cleanly to unblurred tints
 *   (85% smoothstep gradient for soft, 95% flat tint + hairline for hard)
 *   without touching RenderNode or RenderEffect.
 */
final class ScrollEdges {
    private static final BlurGeometry.Level[] LEVELS = BlurGeometry.softLevels();

    private final Paint paint = new Paint(Paint.ANTI_ALIAS_FLAG);
    private BlurGeometry.Edge[] edges;
    private float density = 1f;
    private boolean drawing;
    /** Opaque Api31 state, null on API < 31 or when no edge is active. */
    private Object api31;

    boolean any() {
        return BlurGeometry.anyEdge(edges);
    }

    /** Updates the four edges; returns true if anything changed. */
    boolean configure(BlurGeometry.Edge[] next, float density) {
        boolean same = this.density == density && BlurGeometry.edgesEqual(this.edges, next);
        if (same) return false;
        this.edges = next;
        this.density = density;
        if (!any()) release();
        return true;
    }

    /** Releases any GPU RenderNodes held for edge blurring. */
    void release() {
        if (Build.VERSION.SDK_INT >= 31 && api31 != null) {
            Api31.release(api31);
            api31 = null;
        }
    }

    /**
     * Draws scrollFrame (its background, scroller, edge effects, any extra
     * overlay children such as the pull-to-refresh spinner, and foreground).
     */
    void draw(ViewGroup scrollFrame, Canvas canvas, View scroller,
            Backdrop.Host backdropHost, Object backdropState, Backdrop.Callbacks cb) {
        if (!any() || drawing) {
            cb.superDraw(canvas);
            return;
        }
        int w = scrollFrame.getWidth(), h = scrollFrame.getHeight();
        if (w <= 0 || h <= 0 || scroller == null) {
            cb.superDraw(canvas);
            return;
        }
        drawing = true;
        try {
            if (Build.VERSION.SDK_INT >= 31 && canvas.isHardwareAccelerated()) {
                if (api31 == null) api31 = new Api31.State();
                Api31.draw(scrollFrame, canvas, scroller, edges, density, paint,
                        backdropHost, backdropState, cb, (Api31.State) api31);
            } else {
                drawUnblurred(scrollFrame, canvas, scroller, cb, w, h);
            }
        } finally {
            drawing = false;
        }
    }

    /** API < 31 / software fallback: draws scroller, then unblurred tints + hairlines, then extra children. */
    private void drawUnblurred(ViewGroup scrollFrame, Canvas canvas, View scroller,
            Backdrop.Callbacks cb, int w, int h) {
        drawBackground(scrollFrame, canvas);
        long drawingTime = scrollFrame.getDrawingTime();
        cb.drawChild(canvas, scroller, drawingTime);
        for (int side = 0; side < 4; side++) {
            BlurGeometry.Edge e = edges[side];
            if (e == null || !e.active()) continue;
            float bandPx = e.band() * density;
            float[] r = BlurGeometry.bandRect(side, w, h, bandPx);
            float bx = r[0], by = r[1], bw = r[2], bh = r[3];
            if (bw <= 0f || bh <= 0f) continue;
            if (e.effect == Protocol.EDGE_EFFECT_SOFT) {
                drawSoftTint(canvas, paint, side, e, bx, by, bw, bh, BlurGeometry.UNBLURRED_SOFT_TINT);
            } else {
                drawHardOverlay(canvas, paint, side, e, bx, by, bw, bh, density, BlurGeometry.UNBLURRED_HARD_TINT);
            }
        }
        for (int i = 0; i < scrollFrame.getChildCount(); i++) {
            View child = scrollFrame.getChildAt(i);
            if (child != scroller && child.getVisibility() == View.VISIBLE) {
                cb.drawChild(canvas, child, drawingTime);
            }
        }
        cb.superOnDrawForeground(canvas);
    }

    private static void drawBackground(ViewGroup view, Canvas canvas) {
        Drawable bg = view.getBackground();
        if (bg == null) return;
        Rect b = bg.getBounds();
        if (b.isEmpty()) bg.setBounds(0, 0, view.getWidth(), view.getHeight());
        bg.draw(canvas);
    }

    private static void drawSoftTint(Canvas canvas, Paint paint, int side, BlurGeometry.Edge e,
            float bx, float by, float bw, float bh, float most) {
        BlurGeometry.TintStops stops = BlurGeometry.softTint(e.extent, e.tint, most);
        if (stops == null) return;
        float[] line = BlurGeometry.edgeLine(side);
        paint.setStyle(Paint.Style.FILL);
        paint.setColor(0xff000000);
        paint.setShader(new LinearGradient(
                bx + line[0] * bw, by + line[1] * bh,
                bx + line[2] * bw, by + line[3] * bh,
                stops.colors, stops.offsets, Shader.TileMode.CLAMP));
        canvas.drawRect(bx, by, bx + bw, by + bh, paint);
        paint.setShader(null);
    }

    private static void drawHardOverlay(Canvas canvas, Paint paint, int side, BlurGeometry.Edge e,
            float bx, float by, float bw, float bh, float density, float tintAlpha) {
        paint.setStyle(Paint.Style.FILL);
        paint.setShader(null);
        int fill = BlurGeometry.fadeColor(e.tint, tintAlpha);
        if ((fill >>> 24) != 0) {
            paint.setColor(fill);
            canvas.drawRect(bx, by, bx + bw, by + bh, paint);
        }
        if ((e.line >>> 24) != 0) {
            float[] hr = BlurGeometry.hairlineRect(side, bx, by, bw, bh, BlurGeometry.HAIRLINE_WIDTH * density);
            paint.setColor(e.line);
            canvas.drawRect(hr[0], hr[1], hr[0] + hr[2], hr[1] + hr[3], paint);
        }
    }

    /** API 31+ RenderNode / RenderEffect implementation, never loaded on API < 31. */
    private static final class Api31 {
        private Api31() {}

        static final class State {
            final RenderNode content = new RenderNode("nsgo-edge-content");
            /** Reusable per-side blur RenderNodes: up to LEVELS.length for soft, or 1 for hard. */
            final RenderNode[][] bands = new RenderNode[4][LEVELS.length];

            RenderNode bandNode(int side, int level, int w, int h) {
                RenderNode n = bands[side][level];
                if (n == null) {
                    n = new RenderNode("nsgo-edge-blur");
                    bands[side][level] = n;
                }
                n.setPosition(0, 0, w, h);
                n.setClipToBounds(true);
                return n;
            }

            void discardSide(int side, int fromLevel) {
                for (int k = fromLevel; k < LEVELS.length; k++) {
                    if (bands[side][k] != null) {
                        bands[side][k].discardDisplayList();
                        bands[side][k] = null;
                    }
                }
            }

            void release() {
                content.discardDisplayList();
                for (int s = 0; s < 4; s++) discardSide(s, 0);
            }
        }

        static void release(Object state) {
            ((State) state).release();
        }

        static void draw(ViewGroup scrollFrame, Canvas canvas, View scroller,
                BlurGeometry.Edge[] edges, float density, Paint paint,
                Backdrop.Host backdropHost, Object backdropState,
                Backdrop.Callbacks cb, State state) {
            int w = scrollFrame.getWidth(), h = scrollFrame.getHeight();
            long drawingTime = scrollFrame.getDrawingTime();

            // Record ScrollFrame's background + scroller once into content RenderNode.
            state.content.setPosition(0, 0, w, h);
            state.content.setClipToBounds(true);
            RecordingCanvas rc = state.content.beginRecording();
            drawBackground(scrollFrame, rc);
            boolean forward = backdropHost != null && backdropHost.hasBackdropDescendant(scroller);
            if (forward) {
                backdropHost.setInherited(scroller,
                        Backdrop.forwardContainerBackground(scrollFrame, backdropState, scroller));
            }
            rc.enableZ();
            try {
                cb.drawChild(rc, scroller, drawingTime);
            } finally {
                if (forward) backdropHost.setInherited(scroller, null);
            }
            rc.disableZ();
            state.content.endRecording();

            // Composite the sharp scroll content once.
            canvas.drawRenderNode(state.content);

            // Composite each active edge's blurred band + tint (+ hairline for hard).
            for (int side = 0; side < 4; side++) {
                BlurGeometry.Edge e = edges[side];
                if (e == null || !e.active()) {
                    state.discardSide(side, 0);
                    continue;
                }
                float bandPx = e.band() * density;
                float[] r = BlurGeometry.bandRect(side, w, h, bandPx);
                float bx = r[0], by = r[1], bw = r[2], bh = r[3];
                if (bw <= 0f || bh <= 0f) {
                    state.discardSide(side, 0);
                    continue;
                }
                if (e.effect == Protocol.EDGE_EFFECT_SOFT) {
                    float[] line = BlurGeometry.edgeLine(side);
                    for (int k = 0; k < LEVELS.length; k++) {
                        BlurGeometry.Level lv = LEVELS[k];
                        float sigma = lv.total * density;
                        if (sigma <= 0f || !Float.isFinite(sigma)) continue;
                        int[] src = BlurGeometry.paddedBounds(bx, by, bx + bw, by + bh, sigma, 0, 0, w, h);
                        if (src == null) continue;
                        int srcLeft = src[0], srcTop = src[1];
                        int sw = src[2] - srcLeft, sh = src[3] - srcTop;
                        RenderNode levelNode = state.bandNode(side, k, sw, sh);
                        float gx0 = (bx - srcLeft) + line[0] * bw;
                        float gy0 = (by - srcTop) + line[1] * bh;
                        float gx1 = (bx - srcLeft) + line[2] * bw;
                        float gy1 = (by - srcTop) + line[3] * bh;
                        LinearGradient maskShader = new LinearGradient(
                                gx0, gy0, gx1, gy1,
                                new int[] {0xffffffff, 0x00ffffff},
                                new float[] {lv.full, lv.gone},
                                Shader.TileMode.CLAMP);
                        RenderEffect blur = RenderEffect.createBlurEffect(sigma, sigma, Shader.TileMode.CLAMP);
                        RenderEffect mask = RenderEffect.createShaderEffect(maskShader);
                        levelNode.setRenderEffect(RenderEffect.createBlendModeEffect(blur, mask, BlendMode.DST_IN));
                        RecordingCanvas lc = levelNode.beginRecording();
                        lc.translate(-srcLeft, -srcTop);
                        lc.drawRenderNode(state.content);
                        levelNode.endRecording();

                        int save = canvas.save();
                        canvas.clipRect(bx, by, bx + bw, by + bh);
                        canvas.translate(srcLeft, srcTop);
                        canvas.drawRenderNode(levelNode);
                        canvas.restoreToCount(save);
                    }
                    drawSoftTint(canvas, paint, side, e, bx, by, bw, bh, BlurGeometry.EDGE_TINT);
                } else {
                    float sigma = BlurGeometry.HARD_BLUR * density;
                    int[] src = BlurGeometry.paddedBounds(bx, by, bx + bw, by + bh, sigma, 0, 0, w, h);
                    if (src != null && sigma > 0f && Float.isFinite(sigma)) {
                        int srcLeft = src[0], srcTop = src[1];
                        int sw = src[2] - srcLeft, sh = src[3] - srcTop;
                        RenderNode hardNode = state.bandNode(side, 0, sw, sh);
                        hardNode.setRenderEffect(RenderEffect.createBlurEffect(sigma, sigma, Shader.TileMode.CLAMP));
                        RecordingCanvas hc = hardNode.beginRecording();
                        hc.translate(-srcLeft, -srcTop);
                        hc.drawRenderNode(state.content);
                        hardNode.endRecording();

                        int save = canvas.save();
                        canvas.clipRect(bx, by, bx + bw, by + bh);
                        canvas.translate(srcLeft, srcTop);
                        canvas.drawRenderNode(hardNode);
                        canvas.restoreToCount(save);
                    }
                    state.discardSide(side, 1);
                    drawHardOverlay(canvas, paint, side, e, bx, by, bw, bh, density, BlurGeometry.EDGE_TINT);
                }
            }

            // Draw any remaining children (e.g. pull-to-refresh spinner) over the edges.
            canvas.enableZ();
            for (int i = 0; i < scrollFrame.getChildCount(); i++) {
                View child = scrollFrame.getChildAt(i);
                if (child != scroller && child.getVisibility() == View.VISIBLE) {
                    cb.drawChild(canvas, child, drawingTime);
                }
            }
            canvas.disableZ();
            cb.superOnDrawForeground(canvas);
        }
    }
}
