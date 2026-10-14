package metafile

import (
	"bytes"
	"sort"
	"strconv"
	"testing"
)

func TestParseBencodeRequiresCanonicalForm(t *testing.T) {
	valid := map[string]int64{"i0e": 0, "i7e": 7, "i-7e": -7, "i1024e": 1024, "i-9223372036854775808e": -1 << 63}
	for input, want := range valid {
		node, err := parseBencode([]byte(input))
		if err != nil || node.Kind != KindInteger || node.Int != want {
			t.Fatalf("%q: node=%#v err=%v", input, node, err)
		}
	}
	for _, input := range []string{
		"i+5e", "i+0e", "i-0e", "i03e", "i-03e", "i00e", "ie", "i-e", "i+e", "i5 e", "i 5e",
		"i9223372036854775808e",
		"d0:i1e0:i2ee", "d0:i1e0:i1ee", "d1:ai1e1:ai2ee", "d1:bi1e1:ai2ee", "d1:ai1e0:i2ee",
		"d4:infod0:i1e0:i2eee",
		"03:abc", "1:ab",
	} {
		if _, err := parseBencode([]byte(input)); err == nil {
			t.Fatalf("non-canonical bencode %q was accepted", input)
		}
	}
	node, err := parseBencode([]byte("d0:i1e1:ai2ee"))
	if err != nil || len(node.Dict) != 2 || node.Dict[""].Int != 1 || node.Dict["a"].Int != 2 {
		t.Fatalf("empty first key rejected: node=%#v err=%v", node, err)
	}
}

// encodeNode re-encodes a parsed tree. Because accepted input is canonical,
// parse followed by encode must reproduce the input bytes exactly.
func encodeNode(out *bytes.Buffer, node *Node) {
	switch node.Kind {
	case KindInteger:
		out.WriteByte('i')
		out.WriteString(strconv.FormatInt(node.Int, 10))
		out.WriteByte('e')
	case KindBytes:
		out.WriteString(strconv.Itoa(len(node.Bytes)))
		out.WriteByte(':')
		out.Write(node.Bytes)
	case KindList:
		out.WriteByte('l')
		for _, item := range node.List {
			encodeNode(out, item)
		}
		out.WriteByte('e')
	case KindDictionary:
		out.WriteByte('d')
		keys := make([]string, 0, len(node.Dict))
		for key := range node.Dict {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			out.WriteString(strconv.Itoa(len(key)))
			out.WriteByte(':')
			out.WriteString(key)
			encodeNode(out, node.Dict[key])
		}
		out.WriteByte('e')
	}
}
