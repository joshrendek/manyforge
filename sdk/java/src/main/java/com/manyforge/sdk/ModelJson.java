package com.manyforge.sdk;

import com.fasterxml.jackson.databind.*;
import com.fasterxml.jackson.databind.json.JsonMapper;
import com.fasterxml.jackson.datatype.jsr310.JavaTimeModule;
import org.openapitools.jackson.nullable.JsonNullableModule;
import java.io.IOException;
import java.io.InputStream;
import java.util.Iterator;
import java.util.Map;

/** Shared native date, lossless arbitrary JSON, presence and union codecs. */
public final class ModelJson {
    private ModelJson() {}
    public static ObjectMapper createMapper() {
        return JsonMapper.builder()
            .addModule(new JavaTimeModule())
            .addModule(new JsonNullableModule())
            .addModule(new ModelShapeModule())
            .disable(MapperFeature.ALLOW_COERCION_OF_SCALARS)
            .disable(SerializationFeature.WRITE_DATES_AS_TIMESTAMPS)
            .disable(DeserializationFeature.FAIL_ON_UNKNOWN_PROPERTIES)
            .disable(DeserializationFeature.ADJUST_DATES_TO_CONTEXT_TIME_ZONE)
            .enable(DeserializationFeature.USE_BIG_INTEGER_FOR_INTS)
            .enable(DeserializationFeature.USE_BIG_DECIMAL_FOR_FLOATS)
            .build();
    }
    private static final class Schemas {
        private static final JsonNode VALUE = load();
        private static JsonNode load() {
            try (InputStream stream = ModelJson.class.getResourceAsStream("union-schemas.json")) {
                if (stream == null) throw new IllegalStateException("Missing generated union schema metadata");
                return createMapper().readTree(stream);
            } catch (IOException error) {
                throw new ExceptionInInitializerError(error);
            }
        }
    }
    private static final String[] COMPOSITIONS = {"oneOf", "anyOf", "allOf"};
    /** Uses canonical alternative order, preserving a valid typed node before its draft fallback. */
    public static String alternative(String union, JsonNode value) throws IOException {
        JsonNode schema = Schemas.VALUE.get(union);
        JsonNode alternatives = schema.has("oneOf") ? schema.get("oneOf") : schema.get("anyOf");
        String selected = null;
        for (JsonNode candidate : alternatives) {
            if (!matchesSchema(candidate, value)) continue;
            String name = candidate.get("$ref").asText().replace("#/components/schemas/", "");
            if (selected != null) throw new IOException("Payload matches multiple " + union + " alternatives");
            selected = name;
            if (schema.has("anyOf")) break;
        }
        if (selected == null) throw new IOException("Payload does not match " + union);
        return selected;
    }
    /** Selects typed composed alternatives; ordinary response models remain forward-compatible. */
    public static boolean matches(String schemaName, JsonNode value) {
        JsonNode schema = Schemas.VALUE.get(schemaName);
        if (schema == null) throw new IllegalArgumentException("Unknown generated union member: " + schemaName);
        return matchesSchema(schema, value);
    }
    private static boolean matchesSchema(JsonNode schema, JsonNode value) {
        if (schema.has("$ref")) return matches(schema.get("$ref").asText().replace("#/components/schemas/", ""), value);
        if (value.isNull() && schema.path("nullable").asBoolean(false)) return true;
        for (String composition : COMPOSITIONS) {
            if (!schema.has(composition)) continue;
            int matched = 0;
            for (JsonNode alternative : schema.get(composition)) if (matchesSchema(alternative, value)) matched++;
            if (switch (composition) {
                case "allOf" -> matched != schema.get(composition).size();
                case "oneOf" -> matched != 1;
                default -> matched == 0;
            }) return false;
        }
        if (schema.has("enum")) {
            boolean found = false;
            for (JsonNode known : schema.get("enum")) if (known.equals(value)) found = true;
            if (!found) return false;
        }
        String type = schema.path("type").asText("");
        if (switch (type) {
            case "object" -> !value.isObject();
            case "array" -> !value.isArray();
            case "string" -> !value.isTextual();
            case "integer" -> !value.isIntegralNumber();
            case "number" -> !value.isNumber();
            case "boolean" -> !value.isBoolean();
            default -> false;
        }) return false;
        if (value.isObject()) {
            for (JsonNode required : schema.path("required")) if (!value.has(required.asText())) return false;
            Iterator<Map.Entry<String,JsonNode>> properties = schema.path("properties").fields();
            while (properties.hasNext()) {
                Map.Entry<String,JsonNode> property = properties.next();
                JsonNode supplied = value.get(property.getKey());
                if (supplied != null && !matchesSchema(property.getValue(), supplied)) return false;
            }
        }
        if (value.isArray() && schema.has("items")) {
            for (JsonNode item : value) if (!matchesSchema(schema.get("items"), item)) return false;
        }
        return true;
    }
}
