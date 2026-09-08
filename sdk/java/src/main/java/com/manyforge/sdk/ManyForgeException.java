package com.manyforge.sdk;

import com.fasterxml.jackson.databind.JsonNode;
import java.net.http.HttpHeaders;

/** An HTTP error. The default message deliberately contains no response or request data. */
public final class ManyForgeException extends RuntimeException {
    private final int status;
    private final String code, serverMessage, requestId;
    private final HttpHeaders headers;
    private final JsonNode details;
    private final byte[] rawBody;
    private final boolean truncated;
    ManyForgeException(int status, HttpHeaders headers, JsonNode details, byte[] body, boolean truncated) {
        super("ManyForge HTTP error (status " + status + ")");
        this.status = status; this.headers = headers; this.details = details;
        this.rawBody = body.clone(); this.truncated = truncated;
        this.code = text(details, "code");
        String message = text(details, "message");
        this.serverMessage = message != null ? message : text(details, "error");
        this.requestId = headers.firstValue("X-Request-Id").orElse(null);
    }
    private static String text(JsonNode value, String key) {
        return value != null && value.path(key).isTextual() ? value.path(key).asText() : null;
    }
    public int status() { return status; }
    public String code() { return code; }
    public String serverMessage() { return serverMessage; }
    public String requestId() { return requestId; }
    public HttpHeaders headers() { return headers; }
    public JsonNode details() { return details; }
    public byte[] rawBody() { return rawBody.clone(); }
    public boolean truncated() { return truncated; }
}
