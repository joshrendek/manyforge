package com.manyforge.sdk;

import java.lang.annotation.*;

/** Generator-owned required/nullability metadata, applied only during response decoding. */
@Retention(RetentionPolicy.RUNTIME)
@Target(ElementType.TYPE)
public @interface ModelShape {
    String[] required();
    String[] nonNullable();
}
