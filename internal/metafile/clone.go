package metafile

// Clone returns a deep process-local copy of parsed metafile proof material.
// It is intentionally implemented inside this package so unexported piece
// hashes and Merkle proof bytes cannot be lost when a long-lived authority
// needs to reverify content later in the same process.
func (meta *MetaInfo) Clone() *MetaInfo {
	if meta == nil {
		return nil
	}
	result := *meta
	result.Trackers = append([]string(nil), meta.Trackers...)
	result.NameRaw = append([]byte(nil), meta.NameRaw...)
	result.pieceHashes = append([][20]byte(nil), meta.pieceHashes...)
	result.Files = make([]File, len(meta.Files))
	for index := range meta.Files {
		result.Files[index] = meta.Files[index]
		result.Files[index].Path = append([]string(nil), meta.Files[index].Path...)
		result.Files[index].PathRawBase64 = append([]string(nil), meta.Files[index].PathRawBase64...)
		result.Files[index].RawPath = make([][]byte, len(meta.Files[index].RawPath))
		for component := range meta.Files[index].RawPath {
			result.Files[index].RawPath[component] = append([]byte(nil), meta.Files[index].RawPath[component]...)
		}
		result.Files[index].piecesRootRaw = append([]byte(nil), meta.Files[index].piecesRootRaw...)
		result.Files[index].pieceLayerRaw = append([]byte(nil), meta.Files[index].pieceLayerRaw...)
	}
	return &result
}
