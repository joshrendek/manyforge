package com.manyforge.sdk;

import java.util.*;
import java.util.function.Function;

/** Lazy cursor traversal; each iterator owns its cursor history and preserves its caller's filters. */
public final class Pages {
    private Pages() {}
    public static <P,T> Iterable<T> iterate(String initialCursor, Function<String,P> fetch,
                                            Function<P,List<T>> items, Function<P,String> next) {
        return () -> new Iterator<>() {
            private final Set<String> seen = new HashSet<>();
            private Iterator<T> current = Collections.emptyIterator();
            private String cursor = initialCursor;
            private boolean terminal;
            { if (initialCursor != null && !initialCursor.isEmpty()) seen.add(initialCursor); }
            public boolean hasNext() {
                while (!current.hasNext() && !terminal) {
                    P page = fetch.apply(cursor);
                    List<T> values = items.apply(page);
                    if (values == null) throw new PaginationException("Cursor page is missing its items");
                    current = values.iterator();
                    String following = next.apply(page);
                    terminal = following == null || following.isEmpty();
                    if (!terminal && !seen.add(following)) throw new PaginationException("Server repeated a pagination cursor");
                    cursor = following;
                }
                return current.hasNext();
            }
            public T next() { if (!hasNext()) throw new NoSuchElementException(); return current.next(); }
        };
    }
}
