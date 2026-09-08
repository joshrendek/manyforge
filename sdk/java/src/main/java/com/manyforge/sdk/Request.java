package com.manyforge.sdk;

import com.fasterxml.jackson.core.type.TypeReference;
import java.util.List;

/** Relative wire request. The transport encodes path values exactly once and serializes the body once. */
public record Request<T>(String method, String path, List<Parameter> parameters, Object body,
                         String contentType, boolean authenticated, String audience, boolean stream,
                         String bodyKey, TypeReference<T> responseType) {
    public Request { parameters = List.copyOf(parameters); }
    /** Null values are omitted. Form Upload values carry stream ownership and filename. */
    public record Parameter(String name, String location, Object value, String collectionFormat) {}
}
