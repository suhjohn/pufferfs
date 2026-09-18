"""Decode audio/video once, yielding bounded temporary WAV clips for Gemini."""

from contextlib import nullcontext
from pathlib import Path
import subprocess
import tempfile
import threading
import wave

SAMPLE_RATE = 16000
CLIP_SECONDS = 60


def media_clip_seconds(revision):
    if revision != "visual-gemini-3.5-flash-lite-v2":
        raise ValueError("unsupported media extraction revision")
    return CLIP_SECONDS


def media_inputs(path: str, *, clip_seconds: int = CLIP_SECONDS, timeout: int = 3600, ordinals=None):
    if not 1 <= clip_seconds <= 300 or timeout <= 0:
        raise ValueError("invalid media conversion limits")
    # Only local file/pipe protocols: uploaded playlists must not fetch network
    # resources. Select the first audio track explicitly and discard video.
    process = subprocess.Popen([
        "ffmpeg", "-nostdin", "-v", "error", "-protocol_whitelist", "file,pipe",
        "-i", str(Path(path).resolve()), "-map", "0:a:0", "-vn",
        "-ac", "1", "-ar", str(SAMPLE_RATE), "-f", "s16le", "pipe:1",
    ], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    timer = threading.Timer(timeout, process.kill)
    timer.daemon = True
    timer.start()
    samples = 0
    ordinal = 0
    try:
        with tempfile.TemporaryDirectory(prefix="pufferfs-media-") as directory:
            while True:
                clip = Path(directory) / "clip.wav"
                size = 0
                remaining = clip_seconds * SAMPLE_RATE * 2
                data = process.stdout.read(min(65536, remaining))
                if not data:
                    if process.wait() != 0:
                        raise ValueError("media decoding failed or timed out")
                    if not samples:
                        raise ValueError("media contains no decodable audio")
                    break
                selected = ordinals is None or ordinal in ordinals
                # Decode skipped clips for sample-accurate boundaries without
                # recreating their temporary WAVs or uploading them again.
                with (wave.open(str(clip), "wb") if selected else nullcontext()) as output:
                    if output is not None:
                        output.setparams((1, 2, SAMPLE_RATE, 0, "NONE", "not compressed"))
                    while data:
                        if output is not None:
                            output.writeframesraw(data)
                        size += len(data)
                        remaining -= len(data)
                        if not remaining:
                            break
                        data = process.stdout.read(min(65536, remaining))
                if size % 2:
                    raise ValueError("decoder returned an incomplete audio sample")
                if remaining and process.wait() != 0:
                    raise ValueError("media decoding failed or timed out")
                end = samples + size // 2
                if selected:
                    try:
                        yield {"ordinal": ordinal, "path": str(clip), "mime_type": "audio/wav", "location": {
                            "start_seconds": samples / SAMPLE_RATE, "end_seconds": end / SAMPLE_RATE,
                        }}
                    finally:
                        clip.unlink(missing_ok=True)
                samples = end
                ordinal += 1
                if remaining:
                    break
    finally:
        timer.cancel()
        if process.poll() is None:
            process.kill()
        process.wait()
        process.stdout.close()
