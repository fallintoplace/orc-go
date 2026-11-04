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
}
