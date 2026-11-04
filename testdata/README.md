# ORC fixtures

`apache-encrypted.orc` was written with Apache ORC Java 2.3.1. It contains three rows, a nullified fallback for `secret`, ZLIB-compressed AES-CTR 128 encrypted `secret`, and an unencrypted `clear` column. The fixture uses the test master key `0x31` repeated 16 times.

The following small compatibility fixtures come from the Apache ORC repository and are licensed under Apache License 2.0 (see the repository's `LICENSE` file):

- `apache-test1.orc`: `examples/TestOrcFile.test1.orc`; primitive, nested, list, and map values.
- `apache-row-index.orc`: `examples/orc_index_int_string.orc`; integer and string row-group indexes.
- `apache-string-binary-stats.orc`: `examples/TestOrcFile.testStringAndBinaryStatistics.orc`; nullable strings and binary values with file and stripe statistics.
- `apache-stripe-stats.orc`: `examples/TestOrcFile.testStripeLevelStats.orc`; three stripes with file and stripe statistics.

Source: <https://github.com/apache/orc/tree/main/examples>

The following interoperability fixtures were generated with PyArrow 24.0's Apache ORC C++ writer. They use synthetic values and the Apache ORC 0.11 or 0.12 file version:

- `apache-pyarrow-v0-zlib.orc`: ORC 0.11 with nullable scalar and nested values.
- `apache-pyarrow-v1-uncompressed.orc`, `apache-pyarrow-v1-zlib.orc`, `apache-pyarrow-v1-snappy.orc`, `apache-pyarrow-v1-lz4.orc`, and `apache-pyarrow-v1-zstd.orc`: ORC 0.12 with all Arrow-supported ORC scalar and nested types, RLEv2, string dictionaries, and nullable values.

PyArrow and Apache ORC are licensed under Apache License 2.0.
