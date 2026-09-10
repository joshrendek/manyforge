package com.manyforge.sdk;

import java.net.http.HttpClient;
import java.time.Duration;
import java.util.UUID;

/** Explicit-instance management client. Business scopes are immutable and share this transport. */
public final class ManyForgeClient extends Resources.Root implements AutoCloseable {
    private final NativeTransport http;
    private final Auth auth;
    private ManyForgeClient(Builder builder) { this(new NativeTransport(builder.baseUrl, builder.timeout, builder.httpClient,
        builder.accessToken, builder.tokenProvider, builder.session, null, null, null, null)); }
    private ManyForgeClient(NativeTransport transport) { super(transport, null); http = transport; auth = new Auth(transport); }
    @Override public Auth auth() { return auth; }
    public static final class Auth extends Resources.RootAuth {
        private Auth(Transport transport) { super(transport, null); }
        public com.manyforge.sdk.models.TokenPair login(String email, String password) {
            return login(RootAuthLoginParams.builder().loginRequest(new com.manyforge.sdk.models.LoginRequest().email(email).password(password)).build());
        }
        public java.util.concurrent.CompletableFuture<com.manyforge.sdk.models.TokenPair> loginAsync(String email, String password) {
            return loginAsync(RootAuthLoginParams.builder().loginRequest(new com.manyforge.sdk.models.LoginRequest().email(email).password(password)).build());
        }
        public com.manyforge.sdk.models.TokenPair refresh(String token) {
            return refresh(RootAuthRefreshParams.builder().refreshRequest(new com.manyforge.sdk.models.RefreshRequest().refreshToken(token)).build());
        }
        public java.util.concurrent.CompletableFuture<com.manyforge.sdk.models.TokenPair> refreshAsync(String token) {
            return refreshAsync(RootAuthRefreshParams.builder().refreshRequest(new com.manyforge.sdk.models.RefreshRequest().refreshToken(token)).build());
        }
        public void logout(String token) {
            logout(RootAuthLogoutParams.builder().logoutRequest(new com.manyforge.sdk.models.LogoutRequest().refreshToken(token)).build());
        }
        public java.util.concurrent.CompletableFuture<Void> logoutAsync(String token) {
            return logoutAsync(RootAuthLogoutParams.builder().logoutRequest(new com.manyforge.sdk.models.LogoutRequest().refreshToken(token)).build());
        }
    }
    public static Builder builder() { return new Builder(); }
    public Resources.Business business(String id) { return new Resources.Business(http, NativeTransport.required(id, "business id")); }
    public Resources.Business business(UUID id) { return business(id.toString()); }
    @Override public void close() { http.close(); }
    public static final class Builder {
        private String baseUrl, accessToken;
        private TokenProvider tokenProvider;
        private Session session;
        private HttpClient httpClient;
        private Duration timeout = Duration.ofSeconds(30);
        public Builder baseUrl(String value) { baseUrl = value; return this; }
        public Builder accessToken(String value) { accessToken = NativeTransport.required(value, "accessToken"); return this; }
        public Builder tokenProvider(TokenProvider value) { tokenProvider = java.util.Objects.requireNonNull(value); return this; }
        public Builder session(Session value) { session = java.util.Objects.requireNonNull(value); return this; }
        public Builder httpClient(HttpClient value) { httpClient = java.util.Objects.requireNonNull(value); return this; }
        public Builder timeout(Duration value) { timeout = value; return this; }
        public ManyForgeClient build() { return new ManyForgeClient(this); }
    }
}
