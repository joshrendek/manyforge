package com.manyforge.sdk;

/** Invalid server cursor progression, distinct from HTTP and transport failures. */
public final class PaginationException extends RuntimeException {
    public PaginationException(String message) { super(message); }
}
