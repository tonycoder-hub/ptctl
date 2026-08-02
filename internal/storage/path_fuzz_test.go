package storage

import "testing"

func FuzzValidateComponents(f *testing.F) {
	f.Add("movie.mkv")
	f.Add("../escape")
	f.Add("CON")
	f.Add("name:stream")
	f.Add("e\u0301")
	f.Fuzz(func(t *testing.T, component string) {
		_ = ValidateComponents([][]byte{[]byte(component)}, PathSemantics{Windows: true, CaseSensitive: false, UnicodeNormalization: true})
	})
}
