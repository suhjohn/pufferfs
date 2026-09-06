"""Flat OpenDocument cells, streamed one XML row at a time. No evaluation."""

from defusedxml.ElementTree import iterparse

TABLE = "{urn:oasis:names:tc:opendocument:xmlns:table:1.0}"
TEXT = "{urn:oasis:names:tc:opendocument:xmlns:text:1.0}"
OFFICE = "{urn:oasis:names:tc:opendocument:xmlns:office:1.0}"
MAX_ROWS = 1_048_576
MAX_COLUMNS = 16_384
MAX_ROW_BYTES = 8 * 1024 * 1024


def repetition(element, attribute, limit):
    count = int(element.get(attribute, "1"))
    if not 1 <= count <= limit:
        raise ValueError("OpenDocument repetition exceeds supported sheet dimensions")
    return count


def cell_value(cell):
    # Explicit spaces/tabs/breaks are elements, not ordinary XML text.
    text_bytes = 0
    for element in cell.iter():
        text_bytes += len((element.text or "").encode()) + len((element.tail or "").encode())
        if element.tag == TEXT + "s":
            text_bytes += repetition(element, TEXT + "c", MAX_ROW_BYTES)
        if text_bytes > MAX_ROW_BYTES:
            raise ValueError("OpenDocument expanded cell limit exceeded")
    for element in cell.iter():
        if element.tag == TEXT + "s":
            element.text = " " * repetition(element, TEXT + "c", MAX_ROW_BYTES)
        elif element.tag == TEXT + "tab":
            element.text = "\t"
        elif element.tag == TEXT + "line-break":
            element.text = "\n"
    paragraphs = cell.findall(TEXT + "p")
    if paragraphs:
        return "\n".join("".join(p.itertext()) for p in paragraphs)
    # Exporters may omit displayed paragraphs but preserve the cached value.
    for attribute in ("string-value", "value", "boolean-value", "date-value", "time-value"):
        if OFFICE + attribute in cell.attrib:
            return cell.attrib[OFFICE + attribute]
    return cell.get(TABLE + "formula", "")


def fods_rows(path):
    """Yield ((sheet ordinal, name), row number, values); skip empty repeats."""
    stack = []
    active_row = None
    sheet = None
    sheet_number = row_number = 0
    saw_spreadsheet = False
    with open(path, "rb") as source:
        for event, element in iterparse(source, events=("start", "end"), forbid_dtd=True):
            if event == "start":
                stack.append(element)
                if element.tag == OFFICE + "spreadsheet":
                    saw_spreadsheet = True
                if element.tag == TABLE + "table":
                    if sheet is not None:
                        raise ValueError("nested spreadsheet tables are not supported")
                    sheet_number += 1
                    sheet = (sheet_number, element.get(TABLE + "name", f"Sheet{sheet_number}"))
                    row_number = 0
                elif element.tag == TABLE + "table-row":
                    active_row = element
                continue
            if element is active_row:
                copies = repetition(element, TABLE + "number-rows-repeated", MAX_ROWS)
                if row_number + copies > MAX_ROWS:
                    raise ValueError("OpenDocument row limit exceeded")
                values = []
                row_bytes = 0
                for cell in element:
                    if cell.tag not in {TABLE + "table-cell", TABLE + "covered-table-cell"}:
                        continue
                    count = repetition(cell, TABLE + "number-columns-repeated", MAX_COLUMNS)
                    value = cell_value(cell) if cell.tag == TABLE + "table-cell" else ""
                    row_bytes += len(value.encode()) * count
                    if len(values) + count > MAX_COLUMNS or row_bytes > MAX_ROW_BYTES:
                        raise ValueError("OpenDocument expanded row limit exceeded")
                    values.extend([value] * count)
                if sheet is not None and any(values):
                    for offset in range(1, copies + 1):
                        yield sheet, row_number + offset, values
                row_number += copies
                active_row = None
            if element.tag == TABLE + "table":
                sheet = None
            if active_row is None:
                # Remove processed rows and metadata, not just their contents.
                # Otherwise iterparse retains an empty element per source row.
                if len(stack) > 1:
                    stack[-2].remove(element)
                element.clear()
            stack.pop()
    if not saw_spreadsheet:
        raise ValueError("file is not an OpenDocument spreadsheet")
