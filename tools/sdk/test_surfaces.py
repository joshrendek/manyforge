"""Consumer-compatibility boundaries independent of any generated source wording."""
from copy import deepcopy
import unittest

from surfaces import LANGUAGES, compare_surfaces
from surface.java import parse_class


def snapshot(node):
    return {"format": 1, "languages": {language: {"symbols": {"Client": deepcopy(node)}} for language in LANGUAGES}}


def method(name="limit", type_="int", required=False):
    return {"kind": "method", "signatures": [{"params": [{"name": name, "type": type_, "required": required}], "returns": "Page"}]}


class SurfaceCompatibilityTests(unittest.TestCase):
    def setUp(self):
        self.base = snapshot({"kind": "class", "members": {"list": method(), "label": {"kind": "field", "type": "string | UNSET", "required": False}}})

    def test_additive_methods_and_optional_fields_are_compatible(self):
        revision = deepcopy(self.base)
        for language in LANGUAGES:
            members = revision["languages"][language]["symbols"]["Client"]["members"]
            members["create"] = method()
            members["description"] = {"kind": "field", "type": "string | UNSET", "required": False}
        self.assertEqual(compare_surfaces(self.base, revision), [])

    def test_removed_method_and_renamed_keyword_are_breaking(self):
        removed = deepcopy(self.base)
        del removed["languages"]["python"]["symbols"]["Client"]["members"]["list"]
        self.assertTrue(any("removed public member" in error for error in compare_surfaces(self.base, removed)))
        renamed = deepcopy(self.base)
        renamed["languages"]["python"]["symbols"]["Client"]["members"]["list"] = method("page_size")
        self.assertTrue(any("incompatible signature" in error for error in compare_surfaces(self.base, renamed)))

    def test_presence_and_required_additions_are_breaking(self):
        for change in ("required", "nullable", "added"):
            with self.subTest(change=change):
                revision = deepcopy(self.base)
                members = revision["languages"]["python"]["symbols"]["Client"]["members"]
                if change == "required":
                    members["label"]["required"] = True
                elif change == "nullable":
                    members["label"]["type"] = "string | None"
                else:
                    members["tenant"] = {"kind": "field", "type": "UUID", "required": True}
                self.assertNotEqual(compare_surfaces(self.base, revision), [])

    def test_incompatible_return_and_overload_removal_are_breaking(self):
        revision = deepcopy(self.base)
        revision["languages"]["go"]["symbols"]["Client"]["members"]["list"]["signatures"][0]["returns"] = "string"
        self.assertNotEqual(compare_surfaces(self.base, revision), [])
        base = snapshot({"kind": "function", "signatures": [method()["signatures"][0], method(type_="string")["signatures"][0]]})
        revision = snapshot({"kind": "function", "signatures": [method()["signatures"][0]]})
        self.assertNotEqual(compare_surfaces(base, revision), [])
        self.assertEqual(compare_surfaces(revision, base), [])

    def test_optional_trailing_parameter_preserves_existing_calls(self):
        revision = deepcopy(self.base)
        params = revision["languages"]["typescript"]["symbols"]["Client"]["members"]["list"]["signatures"][0]["params"]
        params.append({"name": "options", "type": "RequestOptions", "required": False})
        self.assertEqual(compare_surfaces(self.base, revision), [])
        params[-1]["required"] = True
        self.assertNotEqual(compare_surfaces(self.base, revision), [])

    def test_added_optional_keyword_can_precede_existing_keywords(self):
        base = snapshot(method())
        for language in LANGUAGES:
            base["languages"][language]["symbols"]["Client"]["signatures"][0]["params"][0]["kind"] = "KEYWORD_ONLY"
        revision = deepcopy(base)
        for language in LANGUAGES:
            revision["languages"][language]["symbols"]["Client"]["signatures"][0]["params"].insert(0, {"name": "cursor", "type": "string", "required": False, "kind": "KEYWORD_ONLY"})
        self.assertEqual(compare_surfaces(base, revision), [])

    def test_new_optional_nonnullable_java_field_does_not_break(self):
        base = snapshot({"kind": "class", "members": {}, "required_fields": ["email"], "non_nullable_fields": ["email"], "model_fields": ["email", "label"]})
        revision = deepcopy(base)
        for language in LANGUAGES:
            node = revision["languages"][language]["symbols"]["Client"]
            node["model_fields"].append("count")
            node["non_nullable_fields"].append("count")
        self.assertEqual(compare_surfaces(base, revision), [])
        revision["languages"]["java"]["symbols"]["Client"]["non_nullable_fields"].append("label")
        self.assertNotEqual(compare_surfaces(base, revision), [])

    def test_open_enum_representation_change_is_breaking(self):
        base = snapshot({"kind": "type", "representation": "string", "members": {"ACTIVE": {"kind": "constant", "type": "Status", "value": "active"}}})
        revision = deepcopy(base)
        revision["languages"]["go"]["symbols"]["Client"]["representation"] = "int"
        self.assertNotEqual(compare_surfaces(base, revision), [])

    def test_missing_language_and_empty_baseline_fail_closed(self):
        self.assertNotEqual(compare_surfaces({}, self.base), [])
        missing = deepcopy(self.base)
        del missing["languages"]["java"]
        self.assertNotEqual(compare_surfaces(self.base, missing), [])
        empty = deepcopy(self.base)
        empty["languages"]["python"]["symbols"] = {}
        self.assertNotEqual(compare_surfaces(empty, self.base), [])

    def test_compiled_java_generic_and_required_annotation_changes(self):
        compiled = '''public class com.manyforge.sdk.models.Contact
  minor version: 0
{
  public java.util.List<java.lang.String> getTags();
    descriptor: ()Ljava/util/List;
    flags: (0x0001) ACC_PUBLIC
}
RuntimeVisibleAnnotations:
  0: #28(#29=[s#30],#31=[s#30])
    com.manyforge.sdk.ModelShape(
      required=["email"]
      nonNullable=["email"]
    )
'''
        _, node = parse_class(compiled)
        self.assertEqual(node["required_fields"], ["email"])
        base = snapshot(node)
        _, changed = parse_class(compiled.replace("java.lang.String", "java.lang.Long"))
        self.assertNotEqual(compare_surfaces(base, snapshot(changed)), [])
        _, changed = parse_class(compiled.replace('required=["email"]', 'required=["email","tenant"]'))
        self.assertNotEqual(compare_surfaces(base, snapshot(changed)), [])


if __name__ == "__main__":
    unittest.main()
