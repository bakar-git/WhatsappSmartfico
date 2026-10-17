#!/usr/bin/env python3
"""Package built binaries and create the GitHub .release update manifest."""
import argparse
import hashlib
import json
from pathlib import Path
import re
from urllib.parse import quote
import zipfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True, help="Release version, e.g. 0.3.0")
    parser.add_argument("--repo", default="bakar-git/WhatsappSmartfico", help="GitHub owner/repo")
    parser.add_argument("--asset", action="append", required=True, help="OS-ARCH=path/to/binary (repeatable)")
    parser.add_argument("--output", type=Path, default=Path("dist/release"))
    args = parser.parse_args()
    version = args.version.removeprefix("v")
    if not re.fullmatch(r"(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?", version):
        parser.error("--version must be a semantic version such as 0.3.0")
    if not re.fullmatch(r"[\w.-]+/[\w.-]+", args.repo):
        parser.error("--repo must be owner/repo")
    assets = {}
    for item in args.asset:
        platform, separator, raw_path = item.partition("=")
        if not separator or not re.fullmatch(r"(windows|linux|darwin)-(amd64|arm64)", platform):
            parser.error("--asset must use windows/linux/darwin-amd64/arm64=path")
        if platform in assets:
            parser.error("duplicate platform: " + platform)
        binary = Path(raw_path)
        if not binary.is_file():
            parser.error("binary does not exist: " + raw_path)
        assets[platform] = binary
    args.output.mkdir(parents=True, exist_ok=True)
    manifest = {"version": version, "assets": {}}
    template = Path(__file__).resolve().parents[1] / ".env.example"
    for platform, binary in assets.items():
        windows = platform.startswith("windows-")
        filename = platform.replace("windows-", "win-", 1) + ".zip"
        archive_path = args.output / filename
        with zipfile.ZipFile(archive_path, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as archive:
            archive.write(binary, "smartfico-whatsapp.exe" if windows else "smartfico-whatsapp")
            archive.write(template, ".env")
        checksum = hashlib.sha256(archive_path.read_bytes()).hexdigest()
        manifest["assets"][platform] = {
            "link": f"https://github.com/{args.repo}/releases/download/{quote('v'+version, safe='')}/{filename}",
            "sha256": checksum,
        }
        print(archive_path)
    manifest_path = args.output / ".release"
    manifest_path.write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
    print(manifest_path)


if __name__ == "__main__":
    main()
