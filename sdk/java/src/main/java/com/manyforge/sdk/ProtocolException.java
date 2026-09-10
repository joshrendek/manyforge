package com.manyforge.sdk;

/** The server returned an invalid successful payload. */
public final class ProtocolException extends RuntimeException {
    public ProtocolException() { super("The server returned an invalid successful payload."); }
}
