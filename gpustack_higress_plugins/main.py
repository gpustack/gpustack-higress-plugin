"""Command-line interface and server for GPUStack Higress Plugins."""

import argparse
import logging
import sys
from typing import Optional

import uvicorn
from fastapi import FastAPI

from gpustack_higress_plugins import __version__
from gpustack_higress_plugins.server import router


def create_app(
    title: str = "GPUStack Higress Plugins",
    description: str = "HTTP server for Higress Wasm plugins",
    version: Optional[str] = None,
) -> FastAPI:
    """Create a FastAPI app with the plugin router.

    Args:
        title: App title
        description: App description
        version: App version (defaults to package version)

    Returns:
        Configured FastAPI app
    """
    if version is None:
        version = __version__

    app = FastAPI(
        title=title,
        description=description,
        version=version,
    )
    app.include_router(router)

    @app.get("/")
    async def root():
        return {"status": "ok", "version": version}

    return app


def main(argv: Optional[list] = None) -> int:
    """Main CLI entry point.

    Args:
        argv: Command line arguments (defaults to sys.argv[1:])

    Returns:
        Exit code
    """
    parser = argparse.ArgumentParser(
        description="GPUStack Higress Plugins Server",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
Examples:
  Start server:  gpustack-plugins start --port 8080 --host 0.0.0.0
  Show version:  gpustack-plugins --version
        """,
    )

    parser.add_argument("--version", action="version", version=f"%(prog)s {__version__}")

    subparsers = parser.add_subparsers(dest="command", help="Available commands")

    # Start server command
    start_parser = subparsers.add_parser("start", help="Start the plugin HTTP server")
    start_parser.add_argument(
        "--port", type=int, default=8080, help="Port to listen on (default: 8080)"
    )
    start_parser.add_argument(
        "--host", default="0.0.0.0", help="Host to bind to (default: 0.0.0.0)"
    )
    start_parser.add_argument(
        "--log-level",
        default="info",
        choices=["critical", "error", "warning", "info", "debug"],
        help="Log level (default: info)",
    )

    args = parser.parse_args(argv)

    if args.command == "start":
        app = create_app()
        logging.getLogger("uvicorn.access").addFilter(lambda r: "GET / HTTP" not in r.getMessage())
        uvicorn.run(
            app,
            host=args.host,
            port=args.port,
            log_level=args.log_level,
        )
        return 0
    else:
        parser.print_help()
        return 0


if __name__ == "__main__":
    sys.exit(main())
