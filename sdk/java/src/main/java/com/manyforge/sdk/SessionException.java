package com.manyforge.sdk;

/** This session requires new authentication. */
public final class SessionException extends RuntimeException {
    public SessionException() { super("This session requires new authentication."); }
}
