package com.manyforge.sdk;

import java.time.Duration;

/** Per-call overrides; null timeout inherits the client setting. Cancelling an async future cancels its exchange. */
public record RequestOptions(Duration timeout) {
    public static final RequestOptions DEFAULT = new RequestOptions(null);
    public RequestOptions {
        if (timeout != null && (timeout.isNegative() || timeout.isZero()))
            throw new IllegalArgumentException("timeout must be positive");
    }
}
