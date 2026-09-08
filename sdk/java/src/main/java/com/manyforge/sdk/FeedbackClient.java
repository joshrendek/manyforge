package com.manyforge.sdk;

import java.net.http.HttpClient;
import java.time.Duration;

/** Credential-isolated public client. No management token or session is accepted. */
public final class FeedbackClient extends Resources.Feedback implements AutoCloseable {
    private final NativeTransport http;
    private FeedbackClient(Builder builder) {
        this(new NativeTransport(builder.baseUrl, builder.timeout, builder.httpClient, null, null, null,
            NativeTransport.required(builder.publishableKey, "publishableKey"), null, null, null), builder.publishableKey);
    }
    private FeedbackClient(NativeTransport transport, String key) { super(transport, key); http = transport; }
    public static Builder builder() { return new Builder(); }
    @Override public void close() { http.close(); }
    public static final class Builder {
        private String baseUrl, publishableKey;
        private HttpClient httpClient;
        private Duration timeout = Duration.ofSeconds(30);
        public Builder baseUrl(String value) { baseUrl = value; return this; }
        public Builder publishableKey(String value) { publishableKey = NativeTransport.required(value, "publishableKey"); return this; }
        public Builder timeout(Duration value) { timeout = value; return this; }
        public Builder httpClient(HttpClient value) { httpClient = java.util.Objects.requireNonNull(value); return this; }
        public FeedbackClient build() { return new FeedbackClient(this); }
    }
}
