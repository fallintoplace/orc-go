package org.fallintoplace.orcinterop;

import org.apache.hadoop.conf.Configuration;
import org.apache.hadoop.fs.Path;
import org.apache.hadoop.hive.ql.exec.vector.BytesColumnVector;
import org.apache.hadoop.hive.ql.exec.vector.LongColumnVector;
import org.apache.hadoop.hive.ql.exec.vector.VectorizedRowBatch;
import org.apache.orc.EncryptionAlgorithm;
import org.apache.orc.EncryptionKey;
import org.apache.orc.OrcFile;
import org.apache.orc.OrcProto;
import org.apache.orc.Reader;
import org.apache.orc.RecordReader;
import org.apache.orc.StripeInformation;
import org.apache.orc.impl.HadoopShims;
import org.apache.orc.impl.KeyProvider;
import org.apache.orc.impl.LocalKey;

import javax.crypto.Cipher;
import javax.crypto.spec.IvParameterSpec;
import javax.crypto.spec.SecretKeySpec;
import java.io.File;
import java.io.IOException;
import java.io.RandomAccessFile;
import java.nio.charset.StandardCharsets;
import java.security.Key;
import java.util.Arrays;
import java.util.HashSet;
import java.util.List;
import java.util.Locale;
import java.util.Set;

public final class InteropCheck {
  private static final int ROW_COUNT = 2505;
  private static final int ROW_INDEX_STRIDE = 1000;
  private static final List<String> CODECS = Arrays.asList(
      "none", "zlib", "snappy", "lzo", "lz4", "zstd", "brotli");

  private InteropCheck() {
  }

  public static void main(String[] args) throws Exception {
    if (args.length != 1) {
      throw new IllegalArgumentException("usage: InteropCheck ORC_FILE_DIRECTORY");
    }
    File outputDir = new File(args[0]);
    for (String codec : CODECS) {
      checkDataFile(new File(outputDir, "data-" + codec + "-dictionary.orc"), codec);
    }
    checkDataFile(new File(outputDir, "data-zlib-direct-rlev1.orc"), "zlib");
    checkRowIndexSeek(new File(outputDir, "data-zlib-direct-rlev1.orc"));
    checkEncryptedFile(new File(outputDir, "encrypted-aes-128.orc"),
        "interop-aes-128", EncryptionAlgorithm.AES_CTR_128);
    checkEncryptedFile(new File(outputDir, "encrypted-aes-256.orc"),
        "interop-aes-256", EncryptionAlgorithm.AES_CTR_256);
  }

  private static void checkDataFile(File file, String codec) throws Exception {
    try (Reader reader = openReader(file, null)) {
      require(reader.getNumberOfRows() == ROW_COUNT,
          file.getName() + ": wrong row count " + reader.getNumberOfRows());
      require(reader.getStripes().size() == 2,
          file.getName() + ": wrong stripe count " + reader.getStripes().size());
      require(reader.getRowIndexStride() == ROW_INDEX_STRIDE,
          file.getName() + ": wrong row-index stride " + reader.getRowIndexStride());
      require(reader.getCompressionKind().name().equalsIgnoreCase(codec),
          file.getName() + ": wrong codec " + reader.getCompressionKind());
      checkRows(reader, false);
      if ("none".equals(codec)) {
        checkIndexAndBloomStreams(reader, file);
      }
    }
    System.out.println("Apache ORC Java read " + file.getName());
  }

  private static Reader openReader(File file, KeyProvider keyProvider) throws IOException {
    OrcFile.ReaderOptions options = OrcFile.readerOptions(new Configuration());
    if (keyProvider != null) {
      options.setKeyProvider(keyProvider);
    }
    return OrcFile.createReader(new Path(file.toURI()), options);
  }

  private static void checkRows(Reader reader, boolean encrypted) throws IOException {
    try (RecordReader rows = reader.rows()) {
      VectorizedRowBatch batch = reader.getSchema().createRowBatch(512);
      int rowNumber = 0;
      while (rows.nextBatch(batch)) {
        LongColumnVector ids = (LongColumnVector) batch.cols[0];
        BytesColumnVector second = (BytesColumnVector) batch.cols[1];
        BytesColumnVector third = encrypted ? (BytesColumnVector) batch.cols[2] : null;
        for (int row = 0; row < batch.size; row++, rowNumber++) {
          require(!isNull(ids, row) && ids.vector[row] == rowNumber,
              "row " + rowNumber + ": invalid id value");
          if (encrypted) {
            String expectedSecret = rowNumber % 17 == 0
                ? null : String.format(Locale.ROOT, "secret-%04d", rowNumber);
            String expectedClear = rowNumber % 11 == 0
                ? null : String.format(Locale.ROOT, "clear-%04d", rowNumber);
            require(equalNullable(textAt(second, row), expectedSecret),
                "row " + rowNumber + ": encrypted value differs");
            require(equalNullable(textAt(third, row), expectedClear),
                "row " + rowNumber + ": clear value differs");
          } else {
            String expectedLabel = rowNumber % 11 == 0
                ? null : String.format(Locale.ROOT, "λ-%03d", rowNumber % 37);
            require(equalNullable(textAt(second, row), expectedLabel),
                "row " + rowNumber + ": label differs");
          }
        }
      }
      require(rowNumber == ROW_COUNT, "read " + rowNumber + " rows, want " + ROW_COUNT);
    }
  }

  private static void checkIndexAndBloomStreams(Reader reader, File file) throws IOException {
    try (RandomAccessFile input = new RandomAccessFile(file, "r")) {
      for (StripeInformation stripe : reader.getStripes()) {
        int footerLength = Math.toIntExact(stripe.getFooterLength());
        byte[] footerBytes = new byte[footerLength];
        long footerOffset = stripe.getOffset() + stripe.getIndexLength() + stripe.getDataLength();
        input.seek(footerOffset);
        input.readFully(footerBytes);
        OrcProto.StripeFooter footer = OrcProto.StripeFooter.parseFrom(footerBytes);
        Set<Integer> rowIndexColumns = new HashSet<>();
        Set<Integer> bloomFilterColumns = new HashSet<>();
        for (OrcProto.Stream stream : footer.getStreamsList()) {
          if (!stream.hasColumn()) {
            continue;
          }
          if (stream.getKind() == OrcProto.Stream.Kind.ROW_INDEX) {
            rowIndexColumns.add(stream.getColumn());
          }
          if (stream.getKind() == OrcProto.Stream.Kind.BLOOM_FILTER
              || stream.getKind() == OrcProto.Stream.Kind.BLOOM_FILTER_UTF8) {
            bloomFilterColumns.add(stream.getColumn());
          }
        }
        require(rowIndexColumns.contains(1) && rowIndexColumns.contains(2),
            "stripe is missing row indexes for id or label");
        require(bloomFilterColumns.contains(1) && bloomFilterColumns.contains(2),
            "stripe is missing bloom filters for id or label");
      }
    }
  }

  private static void checkRowIndexSeek(File file) throws Exception {
    try (Reader reader = openReader(file, null);
         RecordReader rows = reader.rows()) {
      long target = 1200;
      rows.seekToRow(target);
      require(rows.getRowNumber() == target, "row-index seek landed at " + rows.getRowNumber());
      VectorizedRowBatch batch = reader.getSchema().createRowBatch(64);
      require(rows.nextBatch(batch) && batch.size > 0, "row-index seek returned no rows");
      LongColumnVector ids = (LongColumnVector) batch.cols[0];
      require(!isNull(ids, 0) && ids.vector[0] == target,
          "row-index seek returned id " + ids.vector[0] + ", want " + target);
    }
  }

  private static void checkEncryptedRowIndexSeek(Reader reader) throws IOException {
    try (RecordReader rows = reader.rows()) {
      long target = 1200;
      rows.seekToRow(target);
      require(rows.getRowNumber() == target, "encrypted row-index seek landed at " + rows.getRowNumber());
      VectorizedRowBatch batch = reader.getSchema().createRowBatch(64);
      require(rows.nextBatch(batch) && batch.size > 0, "encrypted row-index seek returned no rows");
      LongColumnVector ids = (LongColumnVector) batch.cols[0];
      BytesColumnVector secrets = (BytesColumnVector) batch.cols[1];
      String expectedSecret = String.format(Locale.ROOT, "secret-%04d", target);
      require(!isNull(ids, 0) && ids.vector[0] == target,
          "encrypted row-index seek returned id " + ids.vector[0] + ", want " + target);
      require(expectedSecret.equals(textAt(secrets, 0)),
          "encrypted row-index seek returned the wrong secret");
    }
  }

  private static void checkEncryptedFile(File file, String keyName,
      EncryptionAlgorithm algorithm) throws Exception {
    try (Reader reader = openReader(file, new TestKeyProvider())) {
      require(reader.getNumberOfRows() == ROW_COUNT,
          file.getName() + ": wrong row count " + reader.getNumberOfRows());
      require(reader.getStripes().size() == 2,
          file.getName() + ": wrong stripe count " + reader.getStripes().size());
      require(reader.getRowIndexStride() == ROW_INDEX_STRIDE,
          file.getName() + ": wrong row-index stride " + reader.getRowIndexStride());
      EncryptionKey[] keys = reader.getColumnEncryptionKeys();
      require(keys.length == 1 && keys[0].getKeyName().equals(keyName),
          file.getName() + ": encrypted key metadata differs");
      require(keys[0].getAlgorithm() == algorithm,
          file.getName() + ": encrypted algorithm differs");
      checkRows(reader, true);
      checkEncryptedRowIndexSeek(reader);
    }
    System.out.println("Apache ORC Java decrypted " + file.getName());
  }

  private static boolean isNull(LongColumnVector vector, int row) {
    return !vector.noNulls && vector.isNull[row];
  }

  private static boolean isNull(BytesColumnVector vector, int row) {
    return !vector.noNulls && vector.isNull[row];
  }

  private static String textAt(BytesColumnVector vector, int row) {
    if (isNull(vector, row)) {
      return null;
    }
    return new String(vector.vector[row], vector.start[row], vector.length[row], StandardCharsets.UTF_8);
  }

  private static boolean equalNullable(String actual, String expected) {
    return actual == null ? expected == null : actual.equals(expected);
  }

  private static void require(boolean condition, String message) {
    if (!condition) {
      throw new IllegalStateException(message);
    }
  }

  private static final class TestKeyProvider implements KeyProvider {
    @Override
    public List<String> getKeyNames() {
      return Arrays.asList("interop-aes-128", "interop-aes-256");
    }

    @Override
    public HadoopShims.KeyMetadata getCurrentKeyVersion(String name) {
      EncryptionAlgorithm algorithm = name.endsWith("256")
          ? EncryptionAlgorithm.AES_CTR_256 : EncryptionAlgorithm.AES_CTR_128;
      return new HadoopShims.KeyMetadata(name, 1, algorithm);
    }

    @Override
    public LocalKey createLocalKey(HadoopShims.KeyMetadata metadata) {
      throw new UnsupportedOperationException("interop reader does not create local keys");
    }

    @Override
    public Key decryptLocalKey(HadoopShims.KeyMetadata metadata, byte[] encrypted) throws IOException {
      byte keyByte = metadata.getKeyName().endsWith("256") ? (byte) 0x32 : (byte) 0x31;
      byte[] masterKey = new byte[metadata.getAlgorithm().keyLength()];
      Arrays.fill(masterKey, keyByte);
      if (encrypted.length != masterKey.length) {
        throw new IOException("encrypted local key has an unexpected length");
      }
      try {
        // Hadoop ORC wrapping uses the encrypted key bytes as both the IV and ciphertext.
        byte[] iv = Arrays.copyOf(encrypted, 16);
        Cipher cipher = Cipher.getInstance("AES/CTR/NoPadding");
        cipher.init(Cipher.DECRYPT_MODE,
            new SecretKeySpec(masterKey, "AES"), new IvParameterSpec(iv));
        return new SecretKeySpec(cipher.doFinal(encrypted), "AES");
      } catch (Exception error) {
        throw new IOException("cannot unwrap ORC local key", error);
      }
    }

    @Override
    public HadoopShims.KeyProviderKind getKind() {
      return HadoopShims.KeyProviderKind.HADOOP;
    }
  }
}
