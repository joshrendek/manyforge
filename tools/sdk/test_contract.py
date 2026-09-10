"""Consumer contract failures that must stop SDK generation."""
from __future__ import annotations

import copy
import tempfile
import unittest
from pathlib import Path

from contract import ContractError, load, sdk_projection, validate_contract


def document() -> dict:
    return {
        "openapi": "3.0.3", "info": {"title": "Fixture", "version": "1"},
        "servers": [{"url": "/"}], "security": [{"bearerAuth": []}],
        "paths": {"/api/v1/businesses/{id}/contacts": {
            "parameters": [{"name": "id", "in": "path", "required": True, "schema": {"type": "string"}}],
            "get": {"operationId": "listContacts", "tags": ["Contacts"],
                "x-manyforge-audience": "management", "x-manyforge-resource": "contacts",
                "x-manyforge-method": "list", "x-manyforge-business-param": "id",
                "responses": {"200": {"description": "Contacts", "content": {"application/json": {
                    "schema": {"$ref": "#/components/schemas/ContactPage"}}}}}}
        }},
        "components": {
            "securitySchemes": {"bearerAuth": {"type": "http", "scheme": "bearer"}},
            "schemas": {
                "ContactPage": {"type": "object", "properties": {"items": {"type": "array", "items": {"$ref": "#/components/schemas/Contact"}}}},
                "Contact": {"type": "object", "required": ["id"], "properties": {"id": {"type": "string"}}},
                "Unused": {"type": "string"},
            },
        },
    }


class ContractValidationTest(unittest.TestCase):
    def test_duplicate_yaml_is_rejected_before_parsing_can_lose_an_operation(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "duplicate.yaml"
            path.write_text("paths:\n  /api/v1/me: {}\n  /api/v1/me: {}\n", encoding="utf-8")
            with self.assertRaisesRegex(ContractError, "duplicate YAML key"):
                load(path)

    def test_different_parameter_spellings_cannot_hide_duplicate_routes(self):
        doc = document()
        duplicate = copy.deepcopy(next(iter(doc["paths"].values())))
        duplicate["get"]["operationId"] = "anotherContactsList"
        duplicate["get"]["x-manyforge-method"] = "anotherList"
        doc["paths"]["/api/v1/businesses/{businessID}/contacts"] = duplicate
        with self.assertRaisesRegex(ContractError, "duplicate normalized operation"):
            validate_contract(doc)

    def test_unresolved_response_reference_stops_generation(self):
        doc = document()
        del doc["components"]["schemas"]["Contact"]
        with self.assertRaisesRegex(ContractError, "unresolved reference"):
            validate_contract(doc)

    def test_security_does_not_make_a_callback_an_sdk_method(self):
        doc = document()
        doc["paths"]["/api/v1/callback"] = {"post": {
            "operationId": "callback", "tags": ["Callbacks"], "security": [],
            "x-manyforge-audience": "callback", "x-manyforge-resource": "callbacks", "x-manyforge-method": "receive",
            "responses": {"202": {"description": "Received"}},
        }}
        validate_contract(doc)
        projected = sdk_projection(doc)
        self.assertNotIn("/api/v1/callback", projected["paths"])
        self.assertEqual(set(projected["components"]["schemas"]), {"Contact", "ContactPage"})
        validate_contract(projected)


if __name__ == "__main__":
    unittest.main()
