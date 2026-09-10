package com.manyforge.sdk;

import com.fasterxml.jackson.core.JsonParser;
import com.fasterxml.jackson.databind.*;
import com.fasterxml.jackson.databind.deser.BeanDeserializerModifier;
import com.fasterxml.jackson.databind.deser.std.DelegatingDeserializer;
import com.fasterxml.jackson.databind.module.SimpleModule;
import java.io.IOException;

/** Rejects missing required and illegal null response fields without populating request defaults. */
final class ModelShapeModule extends SimpleModule {
    ModelShapeModule() {
        setDeserializerModifier(new BeanDeserializerModifier() {
            @Override public JsonDeserializer<?> modifyDeserializer(DeserializationConfig config,
                    BeanDescription description, JsonDeserializer<?> deserializer) {
                ModelShape shape = description.getBeanClass().getAnnotation(ModelShape.class);
                return shape == null ? deserializer : new ShapeDeserializer(deserializer, shape);
            }
        });
    }
    private static final class ShapeDeserializer extends DelegatingDeserializer {
        private final ModelShape shape;
        ShapeDeserializer(JsonDeserializer<?> delegate, ModelShape shape) {
            super(delegate);
            this.shape = shape;
        }
        @Override protected JsonDeserializer<?> newDelegatingInstance(JsonDeserializer<?> delegate) {
            return new ShapeDeserializer(delegate, shape);
        }
        @Override public Object deserialize(JsonParser parser, DeserializationContext context) throws IOException {
            JsonNode tree = parser.getCodec().readTree(parser);
            if (!tree.isObject()) throw JsonMappingException.from(parser, "Expected a model object");
            for (String required : shape.required()) {
                if (!tree.has(required)) throw JsonMappingException.from(parser, "Missing required model field: " + required);
            }
            for (String nonNullable : shape.nonNullable()) {
                if (tree.has(nonNullable) && tree.get(nonNullable).isNull())
                    throw JsonMappingException.from(parser, "Null is not allowed for model field: " + nonNullable);
            }
            try (JsonParser fields = tree.traverse(parser.getCodec())) {
                fields.nextToken();
                return _delegatee.deserialize(fields, context);
            }
        }
    }
}
