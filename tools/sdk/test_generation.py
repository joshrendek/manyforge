"""Generation must detect complete file-set drift without touching handwritten code."""
from pathlib import Path
import tempfile
import unittest

from generate import synchronize


class GeneratedOwnershipTest(unittest.TestCase):
    def test_untracked_addition_deletion_and_content_change_fail_the_gate(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "sdk").mkdir()
            model = Path("sdk/go/model_ticket.go")
            files = {model: b"package manyforge\n"}
            synchronize(root, files, "2026.9.1", False)
            extra = root / "sdk/go/model_untracked.go"
            extra.write_text("package manyforge\n")
            with self.assertRaisesRegex(RuntimeError, "unexpected package file"):
                synchronize(root, files, "2026.9.1", True)
            extra.unlink()
            (root / model).unlink()
            with self.assertRaisesRegex(RuntimeError, "missing generated file"):
                synchronize(root, files, "2026.9.1", True)
            (root / model).write_text("package altered\n")
            with self.assertRaisesRegex(RuntimeError, "generated content differs"):
                synchronize(root, files, "2026.9.1", True)

    def test_generation_removes_obsolete_owned_files_and_retains_runtime(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            runtime = root / "sdk/go/runtime_transport.go"
            runtime.parent.mkdir(parents=True)
            runtime.write_text("package manyforge\n// handwritten transport\n")
            old_model = Path("sdk/go/model_old.go")
            new_model = Path("sdk/go/model_new.go")
            synchronize(root, {old_model: b"package manyforge\n"}, "2026.9.1", False)
            synchronize(root, {new_model: b"package manyforge\n"}, "2026.9.2", False)
            self.assertFalse((root / old_model).exists())
            self.assertEqual(runtime.read_text(), "package manyforge\n// handwritten transport\n")
            synchronize(root, {new_model: b"package manyforge\n"}, "2026.9.2", True)

    def test_generator_cannot_take_over_a_handwritten_file(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            path = Path("sdk/python/src/manyforge/transport.py")
            target = root / path
            target.parent.mkdir(parents=True)
            target.write_text("handwritten transport\n")
            with self.assertRaisesRegex(RuntimeError, "refusing to overwrite"):
                synchronize(root, {path: b"generated replacement\n"}, "2026.9.1", False)
            self.assertEqual(target.read_text(), "handwritten transport\n")


if __name__ == "__main__":
    unittest.main()
