"""Encode synthetic AMR-NB input; no downloaded speech or extension renaming.

Uses the OpenCORE C encoder shipped by Debian, only in the fixture runner.
ABI: opencore-amr/amrnb/interf_enc.h; frame framing: test/amrnb-enc.c.
"""

import ctypes
import struct
import subprocess


def create_amr(source, destination):
    pcm = subprocess.run(["ffmpeg", "-nostdin", "-v", "error", "-i", str(source),
        "-ac", "1", "-ar", "8000", "-f", "s16le", "pipe:1"], check=True, capture_output=True).stdout
    assert pcm and len(pcm) % 2 == 0 and len(pcm) <= 1 << 20
    codec = ctypes.CDLL("libopencore-amrnb.so.0")
    codec.Encoder_Interface_init.argtypes = [ctypes.c_int]
    codec.Encoder_Interface_init.restype = ctypes.c_void_p
    codec.Encoder_Interface_exit.argtypes = [ctypes.c_void_p]
    codec.Encoder_Interface_exit.restype = None
    codec.Encoder_Interface_Encode.argtypes = [ctypes.c_void_p, ctypes.c_int,
        ctypes.POINTER(ctypes.c_short), ctypes.POINTER(ctypes.c_ubyte), ctypes.c_int]
    codec.Encoder_Interface_Encode.restype = ctypes.c_int
    encoder = codec.Encoder_Interface_init(0)
    assert encoder
    try:
        with destination.open("wb") as output:
            output.write(b"#!AMR\n")
            encoded = (ctypes.c_ubyte * 500)()
            for start in range(0, len(pcm), 320):
                frame = pcm[start:start + 320].ljust(320, b"\0")
                samples = (ctypes.c_short * 160)(*struct.unpack("<160h", frame))
                length = codec.Encoder_Interface_Encode(encoder, 7, samples, encoded, 0)
                assert 0 < length <= len(encoded)
                output.write(bytes(encoded[:length]))
    finally:
        codec.Encoder_Interface_exit(encoder)
