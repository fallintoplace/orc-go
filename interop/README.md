# Writer interoperability checks

Run the optional cross-implementation check from the repository root:

```sh
./interop/check-writer.sh
```

It needs Go 1.22 or newer, Python with PyArrow 24.0.0, Maven, and JDK 17 or newer.
Install PyArrow with `python3 -m pip install -r interop/pyarrow/requirements.txt`.
The Java check uses Apache ORC 2.3.1; Maven downloads it if it is not already cached.

The script writes synthetic ORC files to a temporary directory. PyArrow checks
all values for nullable nested rows, lists, maps, binary data, two stripes, and
the supported shared compression codecs. Apache ORC Java reads all seven writer
codecs, checks row-index and bloom-filter streams, seeks into a row group, and
decrypts AES-CTR 128 and 256 files.
