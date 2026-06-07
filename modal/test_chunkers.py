from __future__ import annotations

import unittest

from chunkers import (
    _extract_eml_text,
    _extract_ics_text,
    _extract_vcf_text,
    _format_timestamp,
    _media_time_ranges,
    chunk_structured_file,
    detect_file_type,
)


class ChunkersTest(unittest.TestCase):
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
