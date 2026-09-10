package com.manyforge.sdk;

import java.io.ByteArrayInputStream;
import java.io.InputStream;
import java.util.Objects;

/** A caller-owned input stream, consumed once; the SDK never closes the caller's stream. */
public record Upload(String filename, InputStream stream) {
    public Upload { Objects.requireNonNull(filename); Objects.requireNonNull(stream); }
    public static Upload bytes(String filename, byte[] bytes) {
        return new Upload(filename, new ByteArrayInputStream(bytes));
    }
}
