package com.manyforge.sdk;

import com.manyforge.sdk.models.TokenPair;
import java.time.Duration;
import java.util.Objects;
import java.util.concurrent.*;
import java.util.function.Function;

/** One in-memory owner of a rotating human session. Never share refresh tokens across owners. */
public final class Session {
    private String accessToken, refreshToken;
    private long expiresAt, margin;
    private boolean valid = true;
    private CompletableFuture<String> rotation;
    private final Function<TokenPair, ? extends CompletionStage<Void>> onRotate;
    private Session(TokenPair pair, Function<TokenPair, ? extends CompletionStage<Void>> onRotate) {
        this.onRotate = onRotate;
        replace(pair);
    }
    public static Session fromTokenPair(TokenPair pair) { return new Session(pair, null); }
    public static Session fromTokenPair(TokenPair pair, Function<TokenPair, ? extends CompletionStage<Void>> onRotate) {
        return new Session(pair, Objects.requireNonNull(onRotate));
    }
    public synchronized boolean isValid() { return valid; }
    public synchronized void invalidate() { valid = false; accessToken = null; refreshToken = null; }
    private void replace(TokenPair pair) {
        if (pair == null || pair.getAccessToken() == null || pair.getAccessToken().isBlank()
            || pair.getRefreshToken() == null || pair.getRefreshToken().isBlank()
            || pair.getExpiresIn() == null || pair.getExpiresIn() <= 0) throw new SessionException();
        accessToken = pair.getAccessToken(); refreshToken = pair.getRefreshToken();
        long lifetime = Duration.ofSeconds(pair.getExpiresIn()).toNanos();
        expiresAt = System.nanoTime() + lifetime;
        margin = Math.min(Duration.ofSeconds(30).toNanos(), lifetime / 10);
    }
    synchronized CompletableFuture<String> token(Function<String, CompletableFuture<TokenPair>> refresh) {
        if (!valid) return CompletableFuture.failedFuture(new SessionException());
        if (rotation != null) return rotation.copy();
        if (expiresAt - System.nanoTime() >= margin) return CompletableFuture.completedFuture(accessToken);
        CompletableFuture<String> shared = new CompletableFuture<>();
        rotation = shared;
        try {
            refresh.apply(refreshToken).thenCompose(pair -> {
                // Validate before invoking durable persistence. Save a detached copy so callbacks cannot mutate ownership.
                TokenPair snapshot = new TokenPair().accessToken(pair.getAccessToken()).refreshToken(pair.getRefreshToken()).expiresIn(pair.getExpiresIn());
                synchronized (this) { replace(snapshot); }
                CompletionStage<Void> persisted = onRotate == null ? CompletableFuture.completedFuture(null)
                    : Objects.requireNonNull(onRotate.apply(new TokenPair().accessToken(snapshot.getAccessToken()).refreshToken(snapshot.getRefreshToken()).expiresIn(snapshot.getExpiresIn())));
                return persisted.thenApply(ignored -> snapshot.getAccessToken());
            }).whenComplete((token, failure) -> {
                synchronized (this) {
                    if (failure != null || !valid) {
                        invalidate(); shared.completeExceptionally(new SessionException());
                    } else shared.complete(token);
                    rotation = null;
                }
            });
        } catch (Throwable failure) {
            invalidate(); rotation = null; shared.completeExceptionally(new SessionException());
        }
        // Dependent cancellation never cancels another request's refresh or persistence.
        return shared.copy();
    }
}
