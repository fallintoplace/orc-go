#!/usr/bin/env python3

import sys
from pathlib import Path

import pyarrow.orc as orc


ROW_COUNT = 2505
ARROW_CODECS = {
    "none": "UNCOMPRESSED",
    "zlib": "ZLIB",
    "snappy": "SNAPPY",
    "lz4": "LZ4",
    "zstd": "ZSTD",
}


def expected_row(index):
    label = None if index % 11 == 0 else f"λ-{index % 37:03d}"
    if index % 7 == 0:
        profile = None
    else:
        active = None if index % 5 == 0 else index % 2 == 0
        note = None if index % 9 == 0 else f"profile-{index % 31:03d}"
        profile = {"active": active, "note": note}

    if index % 13 == 0:
        items = None
    elif index % 5 == 0:
        items = []
    else:
        items = [index % 73, None, -(index % 73)]

    if index % 17 == 0:
        attrs = None
    elif index % 5 == 0:
        attrs = []
    else:
        attrs = [("group", index % 29), ("λ", None)]

    return {
        "id": index,
        "label": label,
        "profile": profile,
        "items": items,
        "attrs": attrs,
        "weight": None if index % 13 == 0 else index % 101 / 7 - 6,
        "payload": None if index % 19 == 0 else bytes((index & 255, 0, index >> 8 & 255)),
    }


def check_file(path, expected_compression):
    reader = orc.ORCFile(path)
    assert reader.nrows == ROW_COUNT, f"{path.name}: rows={reader.nrows}, want {ROW_COUNT}"
    assert reader.nstripes == 2, f"{path.name}: stripes={reader.nstripes}, want 2"
    assert reader.row_index_stride == 1000, (
        f"{path.name}: row_index_stride={reader.row_index_stride}, want 1000"
    )
    assert reader.compression.upper() == expected_compression, (
        f"{path.name}: compression={reader.compression}, want {expected_compression}"
    )

    actual_rows = reader.read().to_pylist()
    for index, actual in enumerate(actual_rows):
        expected = expected_row(index)
        assert actual == expected, (
            f"{path.name}: row {index} differs\nactual:   {actual!r}\nexpected: {expected!r}"
        )


def main():
    if len(sys.argv) != 2:
        raise SystemExit("usage: check.py ORC_FILE_DIRECTORY")
    output_dir = Path(sys.argv[1])
    files = []
    for codec, compression in ARROW_CODECS.items():
        files.append((output_dir / f"data-{codec}-dictionary.orc", compression))
    files.append((output_dir / "data-zlib-direct-rlev1.orc", "ZLIB"))

    for path, compression in files:
        check_file(path, compression)
        print(f"PyArrow read {path.name}")


if __name__ == "__main__":
    main()
