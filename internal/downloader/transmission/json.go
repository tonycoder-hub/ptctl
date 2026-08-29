package transmission

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"unicode/utf8"
)

const maxJSONDepth = 16

func decodeRPCResult(body []byte, valueProtocol protocol, expectedID int64) (json.RawMessage, error) {
	fields, err := decodeObject(body, 16)
	if err != nil {
		return nil, fmt.Errorf("decode Transmission RPC response")
	}
	if valueProtocol == protocolJSONRPC {
		version, err := requiredString(fields, "jsonrpc")
		if err != nil || version != "2.0" {
			return nil, fmt.Errorf("decode Transmission JSON-RPC response")
		}
		id, err := requiredInt64(fields, "id")
		if err != nil || id != expectedID {
			return nil, fmt.Errorf("Transmission JSON-RPC response ID mismatch")
		}
		if _, exists := fields["error"]; exists {
			return nil, fmt.Errorf("Transmission RPC method failed")
		}
		result, exists := fields["result"]
		if !exists || !isJSONObject(result) {
			return nil, fmt.Errorf("decode Transmission JSON-RPC result")
		}
		return append(json.RawMessage(nil), result...), nil
	}
	result, err := requiredString(fields, "result")
	if err != nil || result != "success" {
		return nil, fmt.Errorf("Transmission RPC method failed")
	}
	tag, err := requiredInt64(fields, "tag")
	if err != nil || tag != expectedID {
		return nil, fmt.Errorf("Transmission legacy response tag mismatch")
	}
	arguments, exists := fields["arguments"]
	if !exists || !isJSONObject(arguments) {
		return nil, fmt.Errorf("decode Transmission legacy result")
	}
	return append(json.RawMessage(nil), arguments...), nil
}

func decodeSessionVersion(raw json.RawMessage, valueProtocol protocol) (string, string, error) {
	fields, err := decodeObject(raw, 256)
	if err != nil {
		return "", "", fmt.Errorf("decode Transmission session information")
	}
	version, err := requiredString(fields, "version")
	if err != nil || version == "" || len(version) > maxVersionBytes || !validControlSafeUTF8(version) {
		return "", "", fmt.Errorf("Transmission returned an invalid version")
	}
	rpcKey := "rpc-version-semver"
	if valueProtocol == protocolJSONRPC {
		rpcKey = "rpc_version_semver"
	}
	rpcVersion, err := requiredString(fields, rpcKey)
	if err != nil || len(rpcVersion) > maxVersionBytes {
		return "", "", fmt.Errorf("Transmission returned an invalid RPC version")
	}
	return version, rpcVersion, nil
}

func decodeObject(data []byte, maxFields int) (map[string]json.RawMessage, error) {
	if maxFields <= 0 || errStrictJSONStrings(data, maxJSONDepth) != nil {
		return nil, fmt.Errorf("invalid JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("invalid JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		if len(fields) >= maxFields {
			return nil, fmt.Errorf("JSON object has too many fields")
		}
		token, err = decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("invalid JSON object field")
		}
		key, ok := token.(string)
		if !ok || key == "" || len(key) > 256 || !validControlSafeUTF8(key) {
			return nil, fmt.Errorf("invalid JSON object field")
		}
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("duplicate JSON object field")
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, fmt.Errorf("invalid JSON object value")
		}
		fields[key] = append(json.RawMessage(nil), raw...)
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, fmt.Errorf("invalid JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("trailing JSON value")
	}
	return fields, nil
}

func decodeRawArray(data []byte, maxItems int, consume func(int, json.RawMessage) error) (int, error) {
	if maxItems <= 0 {
		return 0, fmt.Errorf("invalid JSON array limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return 0, fmt.Errorf("invalid JSON array")
	}
	count := 0
	for decoder.More() {
		if count >= maxItems {
			return count, fmt.Errorf("JSON array exceeds its item limit")
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return count, fmt.Errorf("invalid JSON array item")
		}
		if err := consume(count, raw); err != nil {
			return count + 1, err
		}
		count++
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim(']') {
		return count, fmt.Errorf("invalid JSON array")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return count, fmt.Errorf("trailing JSON value")
	}
	return count, nil
}

func requiredString(fields map[string]json.RawMessage, key string) (string, error) {
	raw, exists := fields[key]
	if !exists {
		return "", fmt.Errorf("missing string field")
	}
	return decodeJSONString(raw)
}

func requiredInt64(fields map[string]json.RawMessage, key string) (int64, error) {
	raw, exists := fields[key]
	if !exists {
		return 0, fmt.Errorf("missing integer field")
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, fmt.Errorf("invalid integer field")
	}
	return value, nil
}

func requiredFloat64(fields map[string]json.RawMessage, key string) (float64, error) {
	raw, exists := fields[key]
	if !exists {
		return 0, fmt.Errorf("missing number field")
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("invalid number field")
	}
	return value, nil
}

func requiredBool(fields map[string]json.RawMessage, key string) (bool, error) {
	raw, exists := fields[key]
	if !exists {
		return false, fmt.Errorf("missing boolean field")
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, fmt.Errorf("invalid boolean field")
	}
	return value, nil
}

func requiredLegacyWanted(fields map[string]json.RawMessage, key string) (bool, error) {
	value, err := requiredInt64(fields, key)
	if err != nil || value != 0 && value != 1 {
		return false, fmt.Errorf("invalid legacy wanted field")
	}
	return value == 1, nil
}

func decodeJSONString(raw []byte) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) < 2 || trimmed[0] != '"' {
		return "", fmt.Errorf("JSON value is not a string")
	}
	end, err := scanStrictJSONString(trimmed, 0)
	if err != nil || end != len(trimmed) {
		return "", fmt.Errorf("invalid JSON string")
	}
	var value string
	if err := json.Unmarshal(trimmed, &value); err != nil || !utf8.ValidString(value) {
		return "", fmt.Errorf("invalid JSON string")
	}
	return value, nil
}

func isJSONObject(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}'
}

func errStrictJSONStrings(data []byte, maxDepth int) error {
	if maxDepth <= 0 || !utf8.Valid(data) {
		return fmt.Errorf("invalid JSON text")
	}
	depth := 0
	for index := 0; index < len(data); index++ {
		switch data[index] {
		case '"':
			end, err := scanStrictJSONString(data, index)
			if err != nil {
				return err
			}
			index = end - 1
		case '{', '[':
			depth++
			if depth > maxDepth {
				return fmt.Errorf("JSON nesting exceeds its limit")
			}
		case '}', ']':
			depth--
			if depth < 0 {
				return fmt.Errorf("invalid JSON nesting")
			}
		}
	}
	return nil
}

func scanStrictJSONString(data []byte, start int) (int, error) {
	if start >= len(data) || data[start] != '"' {
		return 0, fmt.Errorf("invalid JSON string")
	}
	for index := start + 1; index < len(data); index++ {
		value := data[index]
		switch {
		case value == '"':
			return index + 1, nil
		case value == '\\':
			if index+1 >= len(data) {
				return 0, fmt.Errorf("invalid JSON string escape")
			}
			escape := data[index+1]
			switch escape {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				index++
			case 'u':
				code, ok := jsonHexQuad(data, index+2)
				if !ok {
					return 0, fmt.Errorf("invalid JSON Unicode escape")
				}
				if code >= 0xd800 && code <= 0xdbff {
					if index+11 >= len(data) || data[index+6] != '\\' || data[index+7] != 'u' {
						return 0, fmt.Errorf("unpaired JSON Unicode surrogate")
					}
					low, ok := jsonHexQuad(data, index+8)
					if !ok || low < 0xdc00 || low > 0xdfff {
						return 0, fmt.Errorf("unpaired JSON Unicode surrogate")
					}
					index += 11
				} else if code >= 0xdc00 && code <= 0xdfff {
					return 0, fmt.Errorf("unpaired JSON Unicode surrogate")
				} else {
					index += 5
				}
			default:
				return 0, fmt.Errorf("invalid JSON string escape")
			}
		case value < 0x20:
			return 0, fmt.Errorf("invalid JSON string control character")
		case value >= utf8.RuneSelf:
			_, size := utf8.DecodeRune(data[index:])
			if size == 1 {
				return 0, fmt.Errorf("invalid JSON string encoding")
			}
			index += size - 1
		}
	}
	return 0, fmt.Errorf("unterminated JSON string")
}

func jsonHexQuad(data []byte, start int) (uint16, bool) {
	if start < 0 || start+4 > len(data) {
		return 0, false
	}
	var result uint16
	for index := start; index < start+4; index++ {
		value := data[index]
		result <<= 4
		switch {
		case value >= '0' && value <= '9':
			result |= uint16(value - '0')
		case value >= 'a' && value <= 'f':
			result |= uint16(value-'a') + 10
		case value >= 'A' && value <= 'F':
			result |= uint16(value-'A') + 10
		default:
			return 0, false
		}
	}
	return result, true
}
