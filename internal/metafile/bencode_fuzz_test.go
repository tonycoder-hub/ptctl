package metafile

import "testing"

func FuzzParseBencode(f *testing.F) {
	f.Add([]byte("d4:infod4:name1:x12:piece lengthi1e6:pieces20:00000000000000000000ee"))
	f.Add([]byte("d1:ai1ee"))
	f.Add([]byte("i-0e"))
	f.Add([]byte("999999999999999999999999:"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = parseBencode(data)
	})
}
