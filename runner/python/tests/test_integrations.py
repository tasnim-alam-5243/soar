"""The built-in integrations (integrations/*) load in the runner's Python.

Each action module is imported the way the runner does it, so data files read at import
time are parsed too (dfir_iris/actions/endpoints.yaml once failed in PyYAML, which broke
every IRIS action), and every manifest entrypoint must name a function of its module.
"""
import glob
import importlib.util
import os
import sys

import pytest
import yaml

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))  # soar_sdk
ROOT = os.path.abspath(os.path.join(HERE, "..", "..", "..", "integrations"))
MODULES = sorted(glob.glob(os.path.join(ROOT, "*", "actions", "*.py")))


def load(path):
    name = "itest_" + os.path.relpath(path, ROOT).replace(os.sep, "_").removesuffix(".py")
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def test_integrations_are_found():
    assert len(MODULES) >= 3, MODULES


@pytest.mark.parametrize("path", MODULES, ids=lambda p: os.path.relpath(p, ROOT))
def test_action_module_imports(path):
    load(path)


def entrypoints():
    out = []
    for manifest in sorted(glob.glob(os.path.join(ROOT, "*", "manifest.yaml"))):
        with open(manifest, encoding="utf-8") as fh:
            doc = yaml.safe_load(fh)
        for action in doc.get("actions") or []:
            if action.get("entrypoint"):
                out.append((os.path.dirname(manifest), action["id"], action["entrypoint"]))
    return out


ENTRYPOINTS = entrypoints()


@pytest.mark.parametrize("pack,action_id,entrypoint", ENTRYPOINTS, ids=[e[1] for e in ENTRYPOINTS])
def test_manifest_entrypoint_exists(pack, action_id, entrypoint):
    file, _, func = entrypoint.partition(":")
    mod = load(os.path.join(pack, file))
    assert callable(getattr(mod, func, None)), f"{action_id}: {entrypoint} has no function {func}"
