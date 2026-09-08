package com.manyforge.sdk;

import java.util.concurrent.CompletableFuture;

/** Credential-owned request transport. Implement async with HttpClient.sendAsync, never a blocking wrapper. */
public interface Transport {
    <T> T send(Request<T> request, RequestOptions options);
    <T> CompletableFuture<T> sendAsync(Request<T> request, RequestOptions options);
}
