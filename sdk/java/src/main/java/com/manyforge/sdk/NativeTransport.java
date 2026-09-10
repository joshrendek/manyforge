package com.manyforge.sdk;

import com.fasterxml.jackson.databind.*;
import com.fasterxml.jackson.databind.node.ObjectNode;
import com.manyforge.sdk.models.RefreshRequest;
import com.manyforge.sdk.resources.RootAuthResource;
import java.io.*;
import java.net.*;
import java.net.http.*;
import java.nio.ByteBuffer;
import java.nio.charset.StandardCharsets;
import java.time.*;
import java.util.*;
import java.util.concurrent.*;
import java.util.concurrent.atomic.AtomicReference;
import javax.crypto.Mac;
import javax.crypto.spec.SecretKeySpec;

final class NativeTransport implements Transport, AutoCloseable {
    private static final ObjectMapper JSON = ModelJson.createMapper();
    private static final int ERROR_LIMIT = 65536;
    private final String baseUrl, accessToken, publishableKey, secret, signing, origin;
    private final TokenProvider provider;
    private final Session session;
    private final Duration timeout;
    private final HttpClient client;
    private final ExecutorService executor;
    private final boolean owned;
    private final Set<CompletableFuture<?>> active = ConcurrentHashMap.newKeySet();
    private final Set<InputStream> streams = ConcurrentHashMap.newKeySet();
    private ScheduledExecutorService streamTimer;
    private volatile boolean closed;
    NativeTransport(String baseUrl, Duration timeout, HttpClient client, String token, TokenProvider provider,
                    Session session, String key, String secret, String signing, String origin) {
        this.baseUrl = root(baseUrl); this.timeout = Objects.requireNonNull(timeout);
        if (timeout.isNegative() || timeout.isZero()) throw new IllegalArgumentException("timeout must be positive");
        if ((token != null ? 1 : 0) + (provider != null ? 1 : 0) + (session != null ? 1 : 0) > 1)
            throw new IllegalArgumentException("accessToken, tokenProvider and session are mutually exclusive");
        this.accessToken = token; this.provider = provider; this.session = session;
        this.publishableKey = key; this.secret = secret; this.signing = signing; this.origin = origin == null ? null : root(origin);
        owned = client == null;
        if (owned) {
            executor = Executors.newCachedThreadPool(task -> { Thread thread = new Thread(task, "manyforge-http"); thread.setDaemon(true); return thread; });
            this.client = HttpClient.newBuilder().executor(executor).followRedirects(HttpClient.Redirect.NEVER).connectTimeout(timeout).build();
        } else {
            if (client.followRedirects() != HttpClient.Redirect.NEVER || client.cookieHandler().isPresent() || client.authenticator().isPresent())
                throw new IllegalArgumentException("HTTP client must disable redirects, cookies and authenticators");
            executor = null; this.client = client;
        }
    }
    static String required(String value, String name) {
        if (value == null || value.isBlank()) throw new IllegalArgumentException(name + " must be nonempty");
        return value;
    }
    private static String bearer(String value) {
        required(value, "access token");
        // Validate before the JDK can include a rejected header value in its exception.
        for (int i = 0; i < value.length(); i++) {
            char character = value.charAt(i);
            if (character < 0x21 || character > 0x7e)
                throw new IllegalArgumentException("Access token contains invalid header characters");
        }
        return value;
    }
    private long deadline(RequestOptions options) {
        return System.nanoTime() + (options != null && options.timeout() != null ? options.timeout() : timeout).toNanos();
    }
    private static Duration remaining(long deadline) throws HttpTimeoutException {
        long nanos = deadline - System.nanoTime();
        if (nanos <= 0) throw new HttpTimeoutException("HTTP request timed out");
        return Duration.ofNanos(nanos);
    }
    static String root(String value) {
        try {
            URI uri = URI.create(Objects.requireNonNull(value));
            if (!("https".equalsIgnoreCase(uri.getScheme()) || "http".equalsIgnoreCase(uri.getScheme()))
                || uri.getHost() == null || uri.getRawUserInfo() != null || uri.getRawQuery() != null || uri.getRawFragment() != null
                || uri.getPort() == 0 || uri.getPort() > 65535
                || !(uri.getRawPath().isEmpty() || uri.getRawPath().equals("/"))) throw new IllegalArgumentException();
            return uri.getScheme().toLowerCase(Locale.ROOT) + "://" + uri.getRawAuthority();
        } catch (RuntimeException failure) { throw new IllegalArgumentException("An absolute HTTP(S) instance-root URL without credentials, query or fragment is required"); }
    }
    private CompletableFuture<String> token(Request<?> request) {
        if (!request.authenticated()) return CompletableFuture.completedFuture(null);
        if (publishableKey != null) return CompletableFuture.failedFuture(new IllegalArgumentException("Public clients cannot authenticate management requests"));
        try {
            if (session != null) return session.token(baseUrl, refreshToken -> new RootAuthResource(this, null).refreshAsync(
                RootAuthResource.RootAuthRefreshParams.builder().refreshRequest(new RefreshRequest().refreshToken(refreshToken)).build()));
            if (provider != null) return provider.accessToken().toCompletableFuture().copy().thenApply(value -> required(value, "access token"));
            return CompletableFuture.completedFuture(required(accessToken, "access token"));
        } catch (RuntimeException failure) { return CompletableFuture.failedFuture(failure); }
    }
    @Override public <T> T send(Request<T> request, RequestOptions options) {
        if (closed) throw new IllegalStateException("Client is closed");
        long deadline = deadline(options);
        try {
            String token = token(request).get(remaining(deadline).toNanos(), TimeUnit.NANOSECONDS);
            HttpRequest prepared = prepare(request, deadline, token);
            remaining(deadline);
            return decode(request, client.send(prepared, handler(request.stream())), deadline);
        } catch (InterruptedException failure) { Thread.currentThread().interrupt(); throw new CancellationException("HTTP exchange interrupted"); }
        catch (TimeoutException failure) { return NativeTransport.<RuntimeException, T>raise(new HttpTimeoutException("HTTP request timed out")); }
        catch (ExecutionException failure) { throw unchecked(failure.getCause()); }
        catch (IOException failure) { return NativeTransport.<RuntimeException, T>raise(failure); }
    }
    @Override public <T> CompletableFuture<T> sendAsync(Request<T> request, RequestOptions options) {
        if (closed) return CompletableFuture.failedFuture(new IllegalStateException("Client is closed"));
        long deadline = deadline(options);
        AtomicReference<CompletableFuture<?>> exchange = new AtomicReference<>();
        CompletableFuture<T> result = new CompletableFuture<>();
        ScheduledFuture<?> alarm;
        try {
            alarm = timer().schedule(() -> {
                synchronized (result) { result.completeExceptionally(new HttpTimeoutException("HTTP request timed out")); }
            }, remaining(deadline).toNanos(), TimeUnit.NANOSECONDS);
        } catch (Throwable failure) { return CompletableFuture.failedFuture(failure); }
        result.whenComplete((ignored, failure) -> {
            alarm.cancel(false);
            if (failure != null) {
                CompletableFuture<?> pending = exchange.get();
                if (pending != null) pending.cancel(true);
            }
        });
        token(request).whenComplete((token, failure) -> {
            if (result.isDone()) return;
            if (failure != null) { result.completeExceptionally(unwrap(failure)); return; }
            try {
                if (closed) throw new IllegalStateException("Client is closed");
                HttpRequest prepared = prepare(request, deadline, token);
                CompletableFuture<HttpResponse<Object>> nativeRequest;
                synchronized (result) {
                    if (result.isDone()) return;
                    remaining(deadline);
                    nativeRequest = client.sendAsync(prepared, handler(request.stream()));
                    exchange.set(nativeRequest);
                }
                active.add(nativeRequest);
                if (result.isDone()) nativeRequest.cancel(true);
                nativeRequest.whenComplete((response, error) -> {
                    active.remove(nativeRequest);
                    if (error != null) result.completeExceptionally(unwrap(error));
                    else try {
                        T value = decode(request, response, deadline);
                        if (!result.complete(value) && value instanceof InputStream stream) stream.close();
                    } catch (Throwable invalid) { result.completeExceptionally(invalid); }
                });
            } catch (Throwable error) { result.completeExceptionally(error); }
        });
        return result;
    }
    private static Throwable unwrap(Throwable error) {
        while ((error instanceof CompletionException || error instanceof ExecutionException) && error.getCause() != null) error = error.getCause();
        return error;
    }
    private static RuntimeException unchecked(Throwable error) {
        error = unwrap(error);
        if (error instanceof RuntimeException runtime) return runtime;
        return NativeTransport.<RuntimeException, RuntimeException>raise(error);
    }
    @SuppressWarnings("unchecked") private static <E extends Throwable, T> T raise(Throwable failure) throws E { throw (E)failure; }
    private HttpRequest prepare(Request<?> request, long deadline, String token) throws HttpTimeoutException {
        String path = request.path();
        if (!path.startsWith("/") || path.startsWith("//") || path.contains("?") || path.contains("#")) throw new IllegalArgumentException("Invalid generated relative path");
        List<String> query = new ArrayList<>();
        HttpRequest.Builder builder = HttpRequest.newBuilder();
        List<Request.Parameter> form = new ArrayList<>();
        for (Request.Parameter parameter : request.parameters()) {
            if (parameter.value() == null) continue;
            switch (parameter.location()) {
                case "path" -> {
                    String value = encode(parameter.value());
                    if (value.equals(".") || value.equals("..")) value = value.replace(".", "%2E");
                    path = path.replace("{" + parameter.name() + "}", value);
                }
                case "query" -> {
                    if (parameter.value() instanceof Iterable<?> values) {
                        List<String> parts = new ArrayList<>(); values.forEach(value -> parts.add(String.valueOf(value)));
                        if ("multi".equals(parameter.collectionFormat())) for (String value : parts) query.add(encode(parameter.name()) + "=" + encode(value));
                        else query.add(encode(parameter.name()) + "=" + encode(String.join(switch(parameter.collectionFormat()) { case "ssv" -> " "; case "tsv" -> "\t"; case "pipes" -> "|"; default -> ","; }, parts)));
                    } else query.add(encode(parameter.name()) + "=" + encode(parameter.value()));
                }
                case "header" -> {
                    if (parameter.name().equalsIgnoreCase("Authorization") || parameter.name().equalsIgnoreCase("Cookie")) throw new IllegalArgumentException("Credential headers cannot be supplied as operation parameters");
                    builder.header(parameter.name(), String.valueOf(parameter.value()));
                }
                case "form" -> form.add(parameter);
                default -> throw new IllegalArgumentException("Unsupported parameter location");
            }
        }
        if (path.contains("{") || path.contains("}")) throw new IllegalArgumentException("Missing path parameter");
        String target = path + (query.isEmpty() ? "" : "?" + String.join("&", query));
        builder.uri(URI.create(baseUrl + target));
        if (token != null) builder.header("Authorization", "Bearer " + bearer(token));
        if (origin != null) builder.header("Origin", origin);
        byte[] bytes = new byte[0];
        HttpRequest.BodyPublisher body = HttpRequest.BodyPublishers.noBody();
        try {
            if (request.body() != null) {
                if (request.bodyKey() != null) {
                    JsonNode value = JSON.valueToTree(request.body());
                    if (!(value instanceof ObjectNode object)) throw new ProtocolException();
                    object.put(request.bodyKey(), required(publishableKey, "publishableKey"));
                    bytes = JSON.writeValueAsBytes(value);
                } else bytes = JSON.writeValueAsBytes(request.body());
                body = HttpRequest.BodyPublishers.ofByteArray(bytes);
                builder.header("Content-Type", request.contentType());
            } else if (!form.isEmpty()) {
                String boundary = "manyforge-" + UUID.randomUUID();
                body = multipart(form, boundary);
                builder.header("Content-Type", "multipart/form-data; boundary=" + boundary);
            }
            if (secret != null) {
                String timestamp = Long.toString(Instant.now().getEpochSecond());
                String prefix = timestamp + "." + request.method() + "." + ("mailing".equals(signing) ? path : target) + ".";
                Mac mac = Mac.getInstance("HmacSHA256");
                mac.init(new SecretKeySpec(secret.getBytes(StandardCharsets.UTF_8), "HmacSHA256"));
                mac.update(prefix.getBytes(StandardCharsets.UTF_8));
                String signature = HexFormat.of().formatHex(mac.doFinal(bytes));
                if ("mailing".equals(signing)) { builder.header("X-Mailing-Timestamp", timestamp); builder.header("X-Mailing-Signature", signature); }
                else builder.header("X-" + ("feedback".equals(signing) ? "Feedback" : "Telemetry") + "-Signature", "t=" + timestamp + ",v1=" + signature);
            }
        } catch (java.security.GeneralSecurityException | IOException failure) { throw new ProtocolException(); }
        return builder.timeout(remaining(deadline)).method(request.method(), body).build();
    }
    private static String encode(Object value) { return URLEncoder.encode(String.valueOf(value), StandardCharsets.UTF_8).replace("+", "%20").replace("*", "%2A").replace("%7E", "~"); }
    private static HttpRequest.BodyPublisher multipart(List<Request.Parameter> fields, String boundary) {
        List<HttpRequest.BodyPublisher> parts = new ArrayList<>();
        for (Request.Parameter field : fields) {
            String disposition = "--" + boundary + "\r\nContent-Disposition: form-data; name=\"" + quoted(field.name()) + "\"";
            if (field.value() instanceof Upload upload) {
                parts.add(HttpRequest.BodyPublishers.ofString(disposition + "; filename=\"" + quoted(upload.filename()) + "\"\r\nContent-Type: application/octet-stream\r\n\r\n"));
                java.util.concurrent.atomic.AtomicBoolean consumed = new java.util.concurrent.atomic.AtomicBoolean();
                parts.add(HttpRequest.BodyPublishers.ofInputStream(() -> {
                    if (!consumed.compareAndSet(false, true)) throw new IllegalStateException("Upload cannot be replayed");
                    return new FilterInputStream(upload.stream()) { @Override public void close() {} };
                }));
            } else parts.add(HttpRequest.BodyPublishers.ofString(disposition + "\r\n\r\n" + field.value()));
            parts.add(HttpRequest.BodyPublishers.ofString("\r\n"));
        }
        parts.add(HttpRequest.BodyPublishers.ofString("--" + boundary + "--\r\n"));
        return HttpRequest.BodyPublishers.concat(parts.toArray(HttpRequest.BodyPublisher[]::new));
    }
    private static String quoted(String value) {
        if (value.indexOf('\r') >= 0 || value.indexOf('\n') >= 0) throw new IllegalArgumentException("Multipart names cannot contain newlines");
        return value.replace("\\", "\\\\").replace("\"", "\\\"");
    }
    private record ErrorBody(byte[] bytes, boolean truncated) {}
    private static HttpResponse.BodyHandler<Object> handler(boolean stream) {
        return info -> {
            if (info.statusCode() < 200 || info.statusCode() >= 300) return new BoundedBody();
            if (stream) return HttpResponse.BodySubscribers.mapping(HttpResponse.BodySubscribers.ofInputStream(), value -> (Object)value);
            return HttpResponse.BodySubscribers.mapping(HttpResponse.BodySubscribers.ofByteArray(), value -> (Object)value);
        };
    }
    private static final class BoundedBody implements HttpResponse.BodySubscriber<Object> {
        private final CompletableFuture<Object> result = new CompletableFuture<>();
        private final ByteArrayOutputStream bytes = new ByteArrayOutputStream();
        private Flow.Subscription subscription;
        public CompletionStage<Object> getBody() { return result; }
        public void onSubscribe(Flow.Subscription value) { subscription = value; value.request(1); }
        public void onNext(List<ByteBuffer> buffers) {
            for (ByteBuffer buffer : buffers) {
                int count = Math.min(buffer.remaining(), ERROR_LIMIT - bytes.size());
                byte[] chunk = new byte[count]; buffer.get(chunk); bytes.writeBytes(chunk);
                if (buffer.hasRemaining()) { subscription.cancel(); result.complete(new ErrorBody(bytes.toByteArray(), true)); return; }
            }
            subscription.request(1);
        }
        public void onError(Throwable failure) { result.completeExceptionally(failure); }
        public void onComplete() { result.complete(new ErrorBody(bytes.toByteArray(), false)); }
    }
    @SuppressWarnings("unchecked") private <T> T decode(Request<T> request, HttpResponse<Object> response, long deadline) {
        if (response.body() instanceof ErrorBody error) {
            JsonNode details = null;
            if (!error.truncated()) try { details = JSON.readTree(error.bytes()); } catch (IOException ignored) {}
            throw new ManyForgeException(response.statusCode(), response.headers(), details, error.bytes(), error.truncated());
        }
        if (request.stream()) return (T)timedStream((InputStream)response.body(), deadline);
        byte[] bytes = (byte[])response.body();
        if (bytes.length == 0) return null;
        try { return JSON.readValue(bytes, request.responseType()); }
        catch (IOException | RuntimeException failure) { throw new ProtocolException(); }
    }
    private synchronized ScheduledExecutorService timer() {
        if (closed) throw new IllegalStateException("Client is closed");
        if (streamTimer == null) streamTimer = Executors.newSingleThreadScheduledExecutor(task -> {
            Thread thread = new Thread(task, "manyforge-request-timeout"); thread.setDaemon(true); return thread;
        });
        return streamTimer;
    }
    private synchronized InputStream timedStream(InputStream input, long deadline) {
        if (closed) {
            try { input.close(); } catch (IOException ignored) {}
            throw new IllegalStateException("Client is closed");
        }
        final class TimedStream extends FilterInputStream {
            private volatile boolean expired;
            private ScheduledFuture<?> timer;
            TimedStream() { super(input); }
            private void checkDeadline() throws HttpTimeoutException {
                if (expired || System.nanoTime() - deadline >= 0) throw new HttpTimeoutException("HTTP response stream timed out");
            }
            @Override public int read() throws IOException {
                checkDeadline();
                try { int value = in.read(); checkDeadline(); if (value < 0) close(); return value; }
                catch (IOException failure) { checkDeadline(); throw failure; }
            }
            @Override public int read(byte[] bytes, int offset, int length) throws IOException {
                checkDeadline();
                try { int count = in.read(bytes, offset, length); checkDeadline(); if (count < 0) close(); return count; }
                catch (IOException failure) { checkDeadline(); throw failure; }
            }
            @Override public long skip(long count) throws IOException {
                checkDeadline();
                try { long skipped = in.skip(count); checkDeadline(); return skipped; }
                catch (IOException failure) { checkDeadline(); throw failure; }
            }
            @Override public void close() throws IOException {
                if (timer != null) timer.cancel(false);
                streams.remove(this); in.close();
            }
            void expire() { expired = true; try { close(); } catch (IOException ignored) {} }
        }
        TimedStream stream = new TimedStream();
        streams.add(stream);
        stream.timer = timer().schedule(stream::expire, Math.max(0, deadline - System.nanoTime()), TimeUnit.NANOSECONDS);
        return stream;
    }
    @Override public void close() {
        synchronized (this) {
            if (closed) return;
            closed = true;
            for (InputStream stream : streams) try { stream.close(); } catch (IOException ignored) {}
            if (streamTimer != null) streamTimer.shutdownNow();
        }
        for (CompletableFuture<?> request : active) request.cancel(true);
        if (owned) {
            // Java 17 has no HttpClient.close; newer JDKs do. Never close an injected pool.
            try { if ((Object)client instanceof AutoCloseable closeable) closeable.close(); }
            catch (Exception ignored) { throw new TransportException(); }
            finally { executor.shutdown(); }
        }
    }
}
