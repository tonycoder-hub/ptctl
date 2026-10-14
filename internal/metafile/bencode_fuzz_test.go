package metafile

import (
	"bytes"
	"testing"
)

func FuzzParseBencode(f *testing.F) {
	f.Add([]byte("d4:infod4:name1:x12:piece lengthi1e6:pieces20:00000000000000000000ee"))
	f.Add([]byte("d1:ai1ee"))
	f.Add([]byte("i-0e"))
	f.Add([]byte("i+5e"))
	f.Add([]byte("d0:i1e0:i2ee"))
	f.Add([]byte("999999999999999999999999:"))
	f.Fuzz(func(t *testing.T, data []byte) {
		node, err := parseBencode(data)
		if err != nil {
			return
		}
		var encoded bytes.Buffer
		encodeNode(&encoded, node)
		if !bytes.Equal(encoded.Bytes(), data) {
			t.Fatalf("accepted bencode is not canonical: %q re-encodes as %q", data, encoded.Bytes())
		}
	})
}
