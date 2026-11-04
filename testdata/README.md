# ORC fixtures

`apache-encrypted.orc` was written with Apache ORC Java 2.3.1. It contains three rows, a nullified fallback for `secret`, ZLIB-compressed AES-CTR 128 encrypted `secret`, and an unencrypted `clear` column. The fixture uses the test master key `0x31` repeated 16 times.
