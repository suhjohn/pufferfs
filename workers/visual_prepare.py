"""Local-only visual preparation. Yields one temporary PNG per provider request.

No text extraction, provider calls or S3 writes belong here. The consumer must
finish reading each yielded path before advancing the iterator; files are
removed on advance, failure, or explicit iterator close.
"""

from __future__ import annotations

import math
from pathlib import Path
import subprocess
import tempfile

from extraction import file_family

MAX_EDGE = 2400
MAX_SVG_BYTES = 32 << 20


def office_pdf(source: str, directory: str, *, timeout: int = 300) -> str:
    output = Path(directory) / "converted"
    output.mkdir()
    profile = Path(directory) / "office-profile"
    shared_packages = Path(directory) / "office-shared-packages"
    shared_packages.mkdir(mode=0o700)
    # An isolated profile prevents concurrent LibreOffice invocations from
    # attaching to another job's process. Highest macro security disables macros.
    # The installation-wide extension cache is separate from UserInstallation:
    # concurrent root processes otherwise race its write probe/initialization.
    # Workers use built-in filters, not administrator-installed shared extensions.
    user = profile / "user"
    user.mkdir(parents=True)
    (user / "registrymodifications.xcu").write_text(
        '<?xml version="1.0" encoding="UTF-8"?>'
        '<oor:items xmlns:oor="http://openoffice.org/2001/registry">'
        '<item oor:path="/org.openoffice.Office.Common/Security/Scripting">'
        '<prop oor:name="MacroSecurityLevel" oor:op="fuse"><value>3</value></prop>'
        '</item></oor:items>', encoding="utf-8",
    )
    subprocess.run([
        "soffice", f"-env:UserInstallation={profile.as_uri()}",
        f"-env:UNO_SHARED_PACKAGES_CACHE={shared_packages.as_uri()}",
        "--headless", "--nologo", "--nodefault", "--norestore",
        "--convert-to", "pdf", "--outdir", str(output), str(Path(source).resolve()),
    ], check=True, timeout=timeout, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    path = output / (Path(source).stem + ".pdf")
    # LibreOffice sometimes exits successfully without producing an output.
    if not path.is_file() or path.stat().st_size == 0:
        raise ValueError("Office conversion produced no PDF")
    return str(path)


def pdf_images(path: str, directory: str, *, ordinals=None):
    import pymupdf

    with pymupdf.open(path) as document:
        if document.needs_pass:
            raise ValueError("encrypted PDF requires a password")
        yield from rendered_images(document, directory, "page_number", ordinals=ordinals)


def rendered_images(document, directory: str, anchor: str, *, ordinals=None):
    import pymupdf

    for ordinal, page in enumerate(document):
        if ordinals is not None and ordinal not in ordinals:
            continue
        dimensions = (page.rect.width, page.rect.height)
        if any(not math.isfinite(edge) or edge <= 0 for edge in dimensions):
            raise ValueError("invalid rendered page dimensions")
        scale = min(2.0, MAX_EDGE / max(dimensions))
        pixmap = page.get_pixmap(matrix=pymupdf.Matrix(scale, scale), colorspace=pymupdf.csRGB, alpha=False)
        image_path = Path(directory) / "page.png"
        try:
            pixmap.save(str(image_path))
            yield {"ordinal": ordinal, "path": str(image_path), "mime_type": "image/png", "location": {anchor: ordinal}}
        finally:
            image_path.unlink(missing_ok=True)


def svg_images(path: str, directory: str, *, ordinals=None):
    import pymupdf
    from defusedxml.ElementTree import fromstring

    with open(path, "rb") as source:
        raw = source.read(MAX_SVG_BYTES + 1)
    if len(raw) > MAX_SVG_BYTES:
        raise ValueError("SVG exceeds 32 MiB preparation limit")
    root = fromstring(raw, forbid_dtd=True)
    if root.tag not in {"svg", "{http://www.w3.org/2000/svg}svg"}:
        raise ValueError("SVG root element required")
    # Standalone captured bytes only. MuPDF is not a browser: reject constructs
    # it cannot faithfully render instead of fetching assets or omitting them.
    for element in root.iter():
        tag = element.tag.rsplit("}", 1)[-1]
        if tag in {"script", "foreignObject"}:
            raise ValueError("SVG script and foreignObject content are unsupported")
        if tag in {"image", "use"}:
            references = [value for key, value in element.attrib.items()
                          if key in {"href", "{http://www.w3.org/1999/xlink}href"}]
            allowed = ("data:image/png;base64,", "data:image/jpeg;base64,") if tag == "image" else ("#",)
            if not references or any(not href.startswith(allowed) for href in references):
                raise ValueError("SVG resources must be embedded PNG/JPEG images or local symbol references")
    # Stream input supplies no filesystem/archive resolver to the renderer.
    with pymupdf.open(stream=raw, filetype="svg") as document:
        yield from rendered_images(document, directory, "frame_number", ordinals=ordinals)


def image_frames(path: str, directory: str, *, ordinals=None):
    from PIL import Image, ImageOps

    if Path(path).suffix.lower() in {".heic", ".heif"}:
        from pillow_heif import register_heif_opener
        register_heif_opener()
    with Image.open(path) as source:
        for ordinal in range(getattr(source, "n_frames", 1)):
            if ordinals is not None and ordinal not in ordinals:
                continue
            source.seek(ordinal)
            frame = ImageOps.exif_transpose(source)
            try:
                frame.thumbnail((MAX_EDGE, MAX_EDGE), Image.Resampling.LANCZOS)
                rgba = frame.convert("RGBA")
                try:
                    with Image.new("RGB", rgba.size, "white") as rgb:
                        rgb.paste(rgba, mask=rgba.getchannel("A"))
                        image_path = Path(directory) / "frame.png"
                        try:
                            rgb.save(image_path, format="PNG")
                            yield {"ordinal": ordinal, "path": str(image_path), "mime_type": "image/png", "location": {"frame_number": ordinal}}
                        finally:
                            image_path.unlink(missing_ok=True)
                finally:
                    rgba.close()
            finally:
                frame.close()


def visual_inputs(path: str, *, ordinals=None):
    family = file_family(path)
    with tempfile.TemporaryDirectory(prefix="pufferfs-visual-") as directory:
        if family in {"document", "presentation"}:
            pdf = office_pdf(path, directory)
            yield from pdf_images(pdf, directory, ordinals=ordinals)
        elif family == "pdf":
            yield from pdf_images(path, directory, ordinals=ordinals)
        elif family == "image":
            if Path(path).suffix.lower() == ".svg":
                yield from svg_images(path, directory, ordinals=ordinals)
            else:
                yield from image_frames(path, directory, ordinals=ordinals)
        else:
            raise ValueError(f"not a visual file: {family}")
