"""HTTP file server for serving Higress Wasm plugins."""

from pathlib import Path

from fastapi import APIRouter, Response
from fastapi.responses import FileResponse

# Base directory for plugins (relative to this file)
BASE_DIR = Path(__file__).parent
PLUGINS_DIR = BASE_DIR / "plugins"

# Allowed files and their media types
_FILE_TYPES = {
    "plugin.wasm": "application/wasm",
    "metadata.txt": "text/plain",
}

# Create API router for plugin endpoints
router = APIRouter(
    prefix="/wasm-plugins",
    tags=["wasm-plugins"],
)


@router.get("/{plugin_name}/{version}/{filename}")
async def serve_plugin_file_endpoint(plugin_name: str, version: str, filename: str):
    """Serve plugin files (plugin.wasm or metadata.txt)."""
    media_type = _FILE_TYPES.get(filename)
    if media_type is None:
        return Response(status_code=404, content="File not found")

    file_path = (PLUGINS_DIR / plugin_name / version / filename).resolve()
    try:
        file_path.relative_to(PLUGINS_DIR.resolve())
    except ValueError:
        return Response(status_code=403, content="Forbidden")

    if not file_path.exists():
        return Response(status_code=404, content="Plugin not found")

    return FileResponse(
        path=file_path,
        media_type=media_type,
        filename=filename,
    )
