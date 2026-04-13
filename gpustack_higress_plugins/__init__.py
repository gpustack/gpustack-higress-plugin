"""GPUStack Higress Plugins - Higress Wasm plugins for GPUStack."""

from pathlib import Path

from gpustack_higress_plugins.server import router

# Prefer importlib.metadata (correct when installed from wheel).
# Fall back to pyproject.toml parsing for development without install.
try:
    from importlib.metadata import PackageNotFoundError, version

    __version__ = version("gpustack-higress-plugins")
except (ImportError, PackageNotFoundError):
    _pyproject = Path(__file__).parent.parent / "pyproject.toml"
    try:
        for _line in _pyproject.read_text().split("\n"):
            if _line.startswith("version ="):
                __version__ = _line.split("=")[1].strip().strip('"')
                break
        else:
            __version__ = "unknown"
    except OSError:
        __version__ = "unknown"

__all__ = [
    "__version__",
    "router",
]
