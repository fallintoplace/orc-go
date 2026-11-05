package orc

import "testing"

func TestParsePostScriptStripeStatisticsLength(t *testing.T) {
	data := appendProtoVarintField(nil, 1, 1)  // footerLength
	data = appendProtoVarintField(data, 6, 9)  // writerVersion
	data = appendProtoVarintField(data, 7, 38) // stripeStatisticsLength

	postScript, err := parsePostScript(data)
	if err != nil {
		t.Fatal(err)
	}
	if postScript.stripeStatisticsLength != 38 {
		t.Fatalf("stripe statistics length = %d, want 38", postScript.stripeStatisticsLength)
	}
	if !postScript.hasWriterVersion || postScript.writerVersion != 9 {
		t.Fatalf("writer version = (%d, %t), want (9, true)", postScript.writerVersion, postScript.hasWriterVersion)
	}
	reader := Reader{postScript: postScript}
	if got, want := reader.WriterMetadata(), (WriterMetadata{WriterVersion: 9, HasWriterVersion: true}); got != want {
		t.Fatalf("WriterMetadata() = %+v, want %+v", got, want)
	}
}

func TestParsePostScriptRejectsMalformedWriterVersion(t *testing.T) {
	tests := []struct {
		name  string
		field []byte
	}{
		{name: "wrong wire type", field: appendProtoBytesField(nil, 6, []byte{1})},
		{name: "overflow", field: appendProtoVarintField(nil, 6, uint64(^uint32(0))+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data := appendProtoVarintField(nil, 1, 1)
			data = append(data, test.field...)
			if _, err := parsePostScript(data); err == nil {
				t.Fatal("parsePostScript() accepted malformed writer version")
			}
		})
	}
}

func TestParsePostScriptWithoutWriterVersion(t *testing.T) {
	postScript, err := parsePostScript(appendProtoVarintField(nil, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if postScript.hasWriterVersion {
		t.Fatalf("writer version = %d, marked present without a field", postScript.writerVersion)
	}
}
