from __future__ import annotations

import unittest

from chunkers import (
    DEFAULT_VLLM_MODELS,
    GeminiRecitationError,
    _extract_eml_text,
    _extract_ics_text,
    _extract_vcf_text,
    _format_timestamp,
    _choose_vllm_model,
    _openai_compatible_base_url,
    _vllm_model_weights,
    _vllm_models,
    _media_time_ranges,
    chunk_structured_file,
    chunk_image,
    detect_file_type,
    image_to_text,
    render_page_jpeg,
)


class ChunkersTest(unittest.TestCase):
    def test_vllm_models_default_to_flash_lite_pair(self):
        import os

        old = os.environ.pop("PUFFERFS_VLLM_MODELS", None)
        try:
            self.assertEqual(
                _vllm_models(),
                [f"{provider}/{model}" for provider, model in DEFAULT_VLLM_MODELS],
            )
        finally:
            if old is not None:
                os.environ["PUFFERFS_VLLM_MODELS"] = old

    def test_vllm_models_can_be_configured(self):
        import os

        old = os.environ.get("PUFFERFS_VLLM_MODELS")
        os.environ["PUFFERFS_VLLM_MODELS"] = " gemini/gemini-a,openai/gpt-test ,, "
        try:
            self.assertEqual(_vllm_models(), ["gemini/gemini-a", "openai/gpt-test"])
        finally:
            if old is None:
                os.environ.pop("PUFFERFS_VLLM_MODELS", None)
            else:
                os.environ["PUFFERFS_VLLM_MODELS"] = old

    def test_vllm_models_support_weights(self):
        import os

        old = os.environ.get("PUFFERFS_VLLM_MODELS")
        os.environ["PUFFERFS_VLLM_MODELS"] = "gemini/gemini-a:10, openai/gpt-test:2, gemini/gemini-c"
        try:
            self.assertEqual(_vllm_models(), ["gemini/gemini-a", "openai/gpt-test", "gemini/gemini-c"])
            self.assertEqual(
                _vllm_model_weights(),
                [("gemini", "gemini-a", 10.0), ("openai", "gpt-test", 2.0), ("gemini", "gemini-c", 1.0)],
            )
        finally:
            if old is None:
                os.environ.pop("PUFFERFS_VLLM_MODELS", None)
            else:
                os.environ["PUFFERFS_VLLM_MODELS"] = old

    def test_vllm_model_selection_filters_unavailable_providers(self):
        import os

        old = os.environ.get("PUFFERFS_VLLM_MODELS")
        os.environ["PUFFERFS_VLLM_MODELS"] = "gemini/gemini-a:10, fireworks/accounts/fireworks/models/vision-test:2"
        try:
            self.assertEqual(_choose_vllm_model({"fireworks"}), ("fireworks", "accounts/fireworks/models/vision-test"))
        finally:
            if old is None:
                os.environ.pop("PUFFERFS_VLLM_MODELS", None)
            else:
                os.environ["PUFFERFS_VLLM_MODELS"] = old

    def test_fireworks_defaults_to_openai_compatible_base_url(self):
        self.assertEqual(_openai_compatible_base_url("fireworks"), "https://api.fireworks.ai/inference/v1")

    def test_gemini_recitation_falls_back_to_gpt_54_nano(self):
        import chunkers
        import os

        old_models = os.environ.get("PUFFERFS_VLLM_MODELS")
        old_openai_key = os.environ.get("OPENAI_API_KEY")
        old_gemini = chunkers._gemini_image_to_text
        old_openai = chunkers._openai_compatible_image_to_text
        calls = []

        def recitation(*args, **kwargs):
            raise GeminiRecitationError("recitation")

        def fallback(provider, image_bytes, model, mime_type):
            calls.append((provider, image_bytes, model, mime_type))
            return "fallback text"

        os.environ["PUFFERFS_VLLM_MODELS"] = "gemini/gemini-2.5-flash-lite:1"
        os.environ["OPENAI_API_KEY"] = "test-key"
        chunkers._gemini_image_to_text = recitation
        chunkers._openai_compatible_image_to_text = fallback
        try:
            self.assertEqual(image_to_text(b"img", "gemini-key", "image/jpeg"), "fallback text")
            self.assertEqual(calls, [("openai", b"img", "gpt-5.4-nano", "image/jpeg")])
        finally:
            chunkers._gemini_image_to_text = old_gemini
            chunkers._openai_compatible_image_to_text = old_openai
            if old_models is None:
                os.environ.pop("PUFFERFS_VLLM_MODELS", None)
            else:
                os.environ["PUFFERFS_VLLM_MODELS"] = old_models
            if old_openai_key is None:
                os.environ.pop("OPENAI_API_KEY", None)
            else:
                os.environ["OPENAI_API_KEY"] = old_openai_key

    def test_chunk_image_uses_placeholder_when_vllm_fails(self):
        import chunkers

        old_available = chunkers._available_image_providers
        old_image_to_text = chunkers.image_to_text

        def fail_vllm(*args, **kwargs):
            raise RuntimeError("provider failed")

        chunkers._available_image_providers = lambda _key: {"gemini"}
        chunkers.image_to_text = fail_vllm
        try:
            chunks = chunk_image(b"img", "root", "scan.png", None, "", "gemini-key")
            self.assertEqual(len(chunks), 1)
            self.assertEqual(chunks[0].content, "[Image: scan.png]")
        finally:
            chunkers._available_image_providers = old_available
            chunkers.image_to_text = old_image_to_text

    def test_render_page_jpeg_uses_configured_dpi_and_quality(self):
        import os

        old_dpi = os.environ.get("PUFFERFS_MODAL_PAGE_IMAGE_DPI")
        old_quality = os.environ.get("PUFFERFS_MODAL_PAGE_IMAGE_JPEG_QUALITY")
        calls = []

        class FakePixmap:
            def tobytes(self, output, jpg_quality):
                calls.append(("tobytes", output, jpg_quality))
                return b"jpeg"

        class FakePage:
            def get_pixmap(self, dpi, alpha):
                calls.append(("get_pixmap", dpi, alpha))
                return FakePixmap()

        os.environ["PUFFERFS_MODAL_PAGE_IMAGE_DPI"] = "144"
        os.environ["PUFFERFS_MODAL_PAGE_IMAGE_JPEG_QUALITY"] = "70"
        try:
            self.assertEqual(render_page_jpeg(FakePage()), b"jpeg")
            self.assertEqual(calls, [("get_pixmap", 144, False), ("tobytes", "jpeg", 70)])
        finally:
            if old_dpi is None:
                os.environ.pop("PUFFERFS_MODAL_PAGE_IMAGE_DPI", None)
            else:
                os.environ["PUFFERFS_MODAL_PAGE_IMAGE_DPI"] = old_dpi
            if old_quality is None:
                os.environ.pop("PUFFERFS_MODAL_PAGE_IMAGE_JPEG_QUALITY", None)
            else:
                os.environ["PUFFERFS_MODAL_PAGE_IMAGE_JPEG_QUALITY"] = old_quality

    def test_page_image_uploads_run_concurrently(self):
        import os
        import threading
        import time

        import app

        old_concurrency = os.environ.get("PUFFERFS_MODAL_PAGE_IMAGE_UPLOAD_CONCURRENCY")
        os.environ["PUFFERFS_MODAL_PAGE_IMAGE_UPLOAD_CONCURRENCY"] = "12"

        class FakeS3:
            def __init__(self):
                self.active = 0
                self.max_active = 0
                self.calls = []
                self.lock = threading.Lock()

            def put_object(self, Bucket, Key, Body, ContentType):
                with self.lock:
                    self.active += 1
                    self.max_active = max(self.max_active, self.active)
                    self.calls.append((Bucket, Key, Body, ContentType))
                time.sleep(0.02)
                with self.lock:
                    self.active -= 1

        fake_s3 = FakeS3()
        uploads = [(i, f"page-{i}.jpg", b"image") for i in range(12)]
        try:
            app._upload_page_images(fake_s3, "bucket", "doc.pdf", uploads)
            self.assertEqual(len(fake_s3.calls), 12)
            self.assertGreater(fake_s3.max_active, 1)
        finally:
            if old_concurrency is None:
                os.environ.pop("PUFFERFS_MODAL_PAGE_IMAGE_UPLOAD_CONCURRENCY", None)
            else:
                os.environ["PUFFERFS_MODAL_PAGE_IMAGE_UPLOAD_CONCURRENCY"] = old_concurrency

    def test_page_image_to_text_prefers_native_text_over_ocr(self):
        import app
        import chunkers

        old_available = chunkers._available_image_providers
        old_image_to_text = chunkers.image_to_text
        vllm_calls = []

        chunkers._available_image_providers = lambda _key: {"gemini"}

        def fail_if_called(*args, **kwargs):
            vllm_calls.append((args, kwargs))
            raise AssertionError("VLLM should not run when native text exists")

        chunkers.image_to_text = fail_if_called
        try:
            text = app.page_image_to_text.local("doc.pdf", 0, b"img", "native text")
            self.assertEqual(text, "native text")
            self.assertEqual(vllm_calls, [])
        finally:
            chunkers._available_image_providers = old_available
            chunkers.image_to_text = old_image_to_text

    def test_page_image_to_text_uses_vllm_when_native_text_is_blank(self):
        import app
        import chunkers
        import os

        old_available = chunkers._available_image_providers
        old_image_to_text = chunkers.image_to_text
        old_gemini_key = os.environ.get("GEMINI_API_KEY")
        calls = []

        chunkers._available_image_providers = lambda _key: {"gemini"}
        os.environ.pop("GEMINI_API_KEY", None)

        def vllm(image_bytes, gemini_key, mime_type):
            calls.append((image_bytes, gemini_key, mime_type))
            return "vllm text"

        chunkers.image_to_text = vllm
        try:
            text = app.page_image_to_text.local("doc.pdf", 1, b"img", " ")
            self.assertEqual(text, "vllm text")
            self.assertEqual(calls, [(b"img", "", "image/jpeg")])
        finally:
            chunkers._available_image_providers = old_available
            chunkers.image_to_text = old_image_to_text
            if old_gemini_key is None:
                os.environ.pop("GEMINI_API_KEY", None)
            else:
                os.environ["GEMINI_API_KEY"] = old_gemini_key

    def test_document_chunking_ocr_only_blank_native_text_pages(self):
        import app

        old_pdf_to_page_images = app.pdf_to_page_images
        old_page_image_to_text = app.page_image_to_text
        old_upload_page_images = app._upload_page_images
        ocr_inputs = []
        uploaded = []
        test_case = self

        class FakePdfToPageImages:
            def remote(self, pdf_bytes, file_path):
                return [
                    {"page_num": 0, "image_bytes": b"image-0", "fallback_text": "native page 0"},
                    {"page_num": 1, "image_bytes": b"image-1", "fallback_text": " "},
                    {"page_num": 2, "image_bytes": b"image-2", "fallback_text": "native page 2"},
                    {"page_num": 3, "image_bytes": b"image-3", "fallback_text": ""},
                ]

        class FakePageImageToText:
            def starmap(self, inputs, order_outputs=True):
                ocr_inputs.extend(inputs)
                test_case.assertTrue(order_outputs)
                return [f"ocr page {page_num}" for _file_path, page_num, _image_bytes, _fallback_text in inputs]

        def fake_upload(_s3_client, _bucket, _file_path, uploads):
            uploaded.extend(uploads)

        app.pdf_to_page_images = FakePdfToPageImages()
        app.page_image_to_text = FakePageImageToText()
        app._upload_page_images = fake_upload
        try:
            chunks = app.chunk_document_with_stage_functions(b"pdf", "root", "doc.pdf", "pdf", object(), "bucket")
            self.assertEqual([chunk.content for chunk in chunks], ["native page 0", "ocr page 1", "native page 2", "ocr page 3"])
            self.assertEqual([input_[1] for input_ in ocr_inputs], [1, 3])
            self.assertEqual([upload[0] for upload in uploaded], [0, 1, 2, 3])
        finally:
            app.pdf_to_page_images = old_pdf_to_page_images
            app.page_image_to_text = old_page_image_to_text
            app._upload_page_images = old_upload_page_images

    def test_detect_file_type_structured_and_media(self):
        self.assertEqual(detect_file_type("mail/message.eml"), "eml")
        self.assertEqual(detect_file_type("mail/message.msg"), "msg")
        self.assertEqual(detect_file_type("contacts/team.vcf"), "vcf")
        self.assertEqual(detect_file_type("calendar/demo.ics"), "ics")
        self.assertEqual(detect_file_type("media/call.mp3"), "audio")
        self.assertEqual(detect_file_type("media/call.wav"), "audio")
        self.assertEqual(detect_file_type("media/demo.mp4"), "video")
        self.assertEqual(detect_file_type("media/demo.mov"), "video")

    def test_extract_eml_text_prefers_body_content(self):
        raw = (
            b"Subject: Roadmap update\r\n"
            b"From: Alice <alice@example.com>\r\n"
            b"To: Bob <bob@example.com>\r\n"
            b"Date: Tue, 01 Jan 2030 10:00:00 +0000\r\n"
            b"Content-Type: text/plain; charset=utf-8\r\n"
            b"\r\n"
            b"The launch moved to Friday after the API review.\r\n"
        )

        text = _extract_eml_text(raw)

        self.assertIn("Subject: Roadmap update", text)
        self.assertIn("The launch moved to Friday", text)

    def test_extract_vcf_text_keeps_contact_content(self):
        raw = b"""BEGIN:VCARD
VERSION:3.0
FN:Alice Example
ORG:Example Co
TITLE:Product Lead
EMAIL:alice@example.com
TEL:+15551234567
END:VCARD
"""

        text = _extract_vcf_text(raw)

        self.assertIn("Name: Alice Example", text)
        self.assertIn("Organization: Example Co", text)
        self.assertIn("Email: alice@example.com", text)


    def test_extract_ics_text_keeps_event_content(self):
        raw = b"""BEGIN:VCALENDAR
BEGIN:VEVENT
SUMMARY:Launch review
DTSTART:20300101T100000Z
DTEND:20300101T110000Z
LOCATION:Zoom
DESCRIPTION:Review the rollout plan and open risks.
END:VEVENT
END:VCALENDAR
"""

        text = _extract_ics_text(raw)

        self.assertIn("Title: Launch review", text)
        self.assertIn("Location: Zoom", text)
        self.assertIn("Description: Review the rollout plan", text)


    def test_chunk_structured_file_returns_normal_chunks(self):
        chunks = chunk_structured_file(
            b"BEGIN:VCARD\nFN:Alice Example\nEMAIL:alice@example.com\nEND:VCARD\n",
            "root",
            "contacts/team.vcf",
            "vcf",
        )

        self.assertEqual(len(chunks), 1)
        self.assertEqual(chunks[0].file_type, "vcf")
        self.assertIn("Alice Example", chunks[0].content)

    def test_long_email_chunks_keep_header_context(self):
        raw = (
            b"Subject: Long planning thread\r\n"
            b"From: Alice <alice@example.com>\r\n"
            b"To: Bob <bob@example.com>\r\n"
            b"Content-Type: text/plain; charset=utf-8\r\n"
            b"\r\n"
            + b"Roadmap detail and launch risk. " * 300
        )

        chunks = chunk_structured_file(raw, "root", "mail/long.eml", "eml")

        self.assertGreater(len(chunks), 1)
        for chunk in chunks:
            self.assertIn("Subject: Long planning thread", chunk.content)
            self.assertIn("Roadmap detail", chunk.content)

    def test_many_contacts_chunk_on_record_boundaries(self):
        contacts = []
        for idx in range(80):
            contacts.append(
                f"BEGIN:VCARD\nFN:Person {idx}\nEMAIL:person{idx}@example.com\nNOTE:{'detail ' * 20}\nEND:VCARD\n"
            )
        chunks = chunk_structured_file("".join(contacts).encode(), "root", "contacts/team.vcf", "vcf")

        self.assertGreater(len(chunks), 1)
        for chunk in chunks:
            self.assertTrue(chunk.content.startswith("Contact "))
            self.assertNotIn("BEGIN:VCARD", chunk.content)

    def test_many_events_chunk_on_record_boundaries(self):
        events = []
        for idx in range(80):
            events.append(
                f"BEGIN:VEVENT\nSUMMARY:Event {idx}\nDESCRIPTION:{'planning detail ' * 20}\nEND:VEVENT\n"
            )
        chunks = chunk_structured_file("".join(events).encode(), "root", "calendar/team.ics", "ics")

        self.assertGreater(len(chunks), 1)
        for chunk in chunks:
            self.assertTrue(chunk.content.startswith("Calendar item "))
            self.assertNotIn("BEGIN:VEVENT", chunk.content)


    def test_media_time_ranges_use_six_minute_windows_with_overlap(self):
        ranges = _media_time_ranges(900)

        self.assertEqual(
            ranges,
            [
                (0, 0.0, 360.0),
                (1, 300.0, 660.0),
                (2, 600.0, 900),
            ],
        )


    def test_format_timestamp_supports_hour_long_media(self):
        self.assertEqual(_format_timestamp(75), "01:15")
        self.assertEqual(_format_timestamp(3670), "01:01:10")


if __name__ == "__main__":
    unittest.main()
