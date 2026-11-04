# ORC fixtures

`apache-encrypted.orc` was written with Apache ORC Java 2.3.1. It contains three rows, a nullified fallback for `secret`, ZLIB-compressed AES-CTR 128 encrypted `secret`, and an unencrypted `clear` column. The fixture uses the test master key `0x31` repeated 16 times.

The following small compatibility fixtures come from the Apache ORC repository and are licensed under Apache License 2.0 (see the repository's `LICENSE` file):

- `apache-test1.orc`: `examples/TestOrcFile.test1.orc`; primitive, nested, list, and map values.
- `apache-string-binary-stats.orc`: `examples/TestOrcFile.testStringAndBinaryStatistics.orc`; nullable strings and binary values with file and stripe statistics.
- `apache-stripe-stats.orc`: `examples/TestOrcFile.testStripeLevelStats.orc`; three stripes with file and stripe statistics.

Source: <https://github.com/apache/orc/tree/main/examples>
