"""Read public descriptors, generic signatures and model annotations from javap."""
from __future__ import annotations

import json
from pathlib import Path
import re
import subprocess
import zipfile


def split_types(value: str) -> list[str]:
    result = []
    depth = 0
    start = 0
    for index, char in enumerate(value):
        if char in "<[":
            depth += 1
        elif char in ">]":
            depth -= 1
        elif char == "," and depth == 0:
            result.append(value[start:index].strip())
            start = index + 1
    if value[start:].strip():
        result.append(value[start:].strip())
    return result


def parse_class(block: str) -> tuple[str, dict] | None:
    header = re.search(r"^(public (?:[^\n]*? )?(?:class|interface|enum) ([\w.$]+)[^\n]*)$", block, re.M)
    if not header:
        return None
    declaration = header.group(1).removesuffix(" {").strip()
    name = header.group(2)
    result = {"kind": "class", "declaration": declaration, "members": {}}
    members = result["members"]
    rows = block.splitlines()
    for index, row in enumerate(rows):
        if not re.match(r"^  (?:public|protected) ", row) or not row.rstrip().endswith(";"):
            continue
        # javap exposes synthetic bridge methods in verbose mode; those are not source declarations.
        attributes = "\n".join(rows[index + 1:index + 5])
        if "ACC_SYNTHETIC" in attributes or "ACC_BRIDGE" in attributes:
            continue
        text = row.strip().removesuffix(";")
        if "(" in text:
            prefix, remaining = text.split("(", 1)
            arguments, suffix = remaining.rsplit(")", 1)
            pieces = prefix.rsplit(" ", 1)
            method = pieces[-1]
            returns = pieces[0] if len(pieces) == 2 else ""
            if method == name:
                method = "<init>"
            node = members.setdefault(method, {"kind": "method", "required": " abstract " in " " + returns + " ", "signatures": []})
            node["signatures"].append({"params": [{"name": str(i), "type": value, "required": not value.endswith("...")} for i, value in enumerate(split_types(arguments))], "returns": returns, "throws": suffix.strip()})
        else:
            field_type, field_name = text.rsplit(" ", 1)
            members[field_name] = {"kind": "field", "type": field_type, "required": False}
            constant = re.search(r"ConstantValue: (?:String|int|long|float|double) (.*)", "\n".join(rows[index + 1:index + 8]))
            if constant and field_name not in ("VERSION", "RELEASE_ID"):
                members[field_name]["value"] = constant.group(1)
    shape = re.search(r"com\.manyforge\.sdk\.ModelShape\(\s*(.*?)\n\s*\)", block, re.S)
    if shape:
        for source, target in (("required", "required_fields"), ("nonNullable", "non_nullable_fields")):
            match = re.search(r"\b" + source + r"=\[(.*?)\]", shape.group(1), re.S)
            result[target] = sorted(re.findall(r'"([^"\n]*)"', match.group(1))) if match else []
        properties = re.search(r"com\.fasterxml\.jackson\.annotation\.JsonPropertyOrder\(\s*value=\[(.*?)\]", block, re.S)
        result["model_fields"] = sorted(re.findall(r'"([^"\n]*)"', properties.group(1))) if properties else sorted(set(result["required_fields"]) | set(result["non_nullable_fields"]))
    for node in members.values():
        if "signatures" in node:
            node["signatures"].sort(key=lambda sig: json.dumps(sig, sort_keys=True))
    return name, result


def extract_java(jar: Path) -> dict:
    with zipfile.ZipFile(jar) as archive:
        classes = sorted(name.removesuffix(".class").replace("/", ".") for name in archive.namelist()
                         if name.startswith("com/manyforge/sdk/") and name.endswith(".class") and not re.search(r"\$\d", name))
    if not classes:
        raise ValueError("Java SDK jar contains no classes")
    symbols = {}
    for offset in range(0, len(classes), 100):
        output = subprocess.run(["javap", "-classpath", str(jar), "-public", "-v", *classes[offset:offset + 100]],
                                check=True, text=True, stdout=subprocess.PIPE).stdout
        for block in re.split(r"(?m)^Classfile ", output):
            parsed = parse_class(block)
            if parsed:
                name, node = parsed
                symbols[name] = node
    if not symbols:
        raise ValueError("javap produced no public SDK descriptors")
    return {"symbols": symbols}
