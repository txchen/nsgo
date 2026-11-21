package android.graphics;

public final class Typeface {
    public static final int NORMAL = 0;
    public static final Typeface DEFAULT = new Typeface("default", 400, false);
    public final String family;
    public final int weight;
    public final boolean italic;

    private Typeface(String family, int weight, boolean italic) {
        this.family = family;
        this.weight = weight;
        this.italic = italic;
    }

    public static Typeface create(String family, int style) {
        return new Typeface(family, 400, false);
    }

    public static Typeface create(Typeface base, int weight, boolean italic) {
        return new Typeface(base == null ? "default" : base.family, weight, italic);
    }
}
