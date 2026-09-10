package com.manyforge.sdk;

/** The HTTP exchange failed; its outcome may be unknown. */
public final class TransportException extends RuntimeException {
    public TransportException() { super("The HTTP exchange failed; its outcome may be unknown."); }
}
