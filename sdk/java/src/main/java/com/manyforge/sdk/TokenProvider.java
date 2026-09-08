package com.manyforge.sdk;

import java.util.concurrent.CompletionStage;

/** Returns the current token for each authenticated request; no token is cached. */
@FunctionalInterface
public interface TokenProvider {
    CompletionStage<String> accessToken();
}
