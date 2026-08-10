package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

const (
	reconciliationCredentialSchema = "ptctl.credentials/v1"
	maxCredentialBundleBytes       = 128 << 10
)

func readReconciliationCredentialBundle(reader io.Reader, username string) (site.Credential, downloader.Credential, error) {
	if err := rejectTTYSecret(reader); err != nil {
		return site.Credential{}, downloader.Credential{}, err
	}
	raw, err := io.ReadAll(io.LimitReader(reader, maxCredentialBundleBytes+1))
	if err != nil {
		return site.Credential{}, downloader.Credential{}, fmt.Errorf("read reconciliation credential bundle from stdin failed")
	}
	if len(raw) == 0 || len(raw) > maxCredentialBundleBytes || !utf8.Valid(raw) {
		return site.Credential{}, downloader.Credential{}, fmt.Errorf("reconciliation credential bundle is invalid")
	}
	values, err := decodeReconciliationCredentialBundle(raw)
	if err != nil {
		return site.Credential{}, downloader.Credential{}, fmt.Errorf("reconciliation credential bundle is invalid")
	}
	if values["schema"] != reconciliationCredentialSchema {
		return site.Credential{}, downloader.Credential{}, fmt.Errorf("reconciliation credential bundle schema is unsupported")
	}
	siteCredential, err := site.NewCookieCredential(values["site_cookie"])
	if err != nil {
		return site.Credential{}, downloader.Credential{}, fmt.Errorf("reconciliation site credential is invalid")
	}
	clientCredential, err := downloader.NewCredential(username, values["downloader_password"])
	if err != nil {
		return site.Credential{}, downloader.Credential{}, fmt.Errorf("reconciliation downloader credential is invalid")
	}
	return siteCredential, clientCredential, nil
}

func decodeReconciliationCredentialBundle(raw []byte) (map[string]string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return nil, fmt.Errorf("credential bundle is not an object")
	}
	values := make(map[string]string, 3)
	for decoder.More() {
		keyToken, err := decoder.Token()
		key, ok := keyToken.(string)
		if err != nil || !ok {
			return nil, fmt.Errorf("credential bundle key is invalid")
		}
		switch key {
		case "schema", "site_cookie", "downloader_password":
		default:
			return nil, fmt.Errorf("credential bundle field is unknown")
		}
		if _, exists := values[key]; exists {
			return nil, fmt.Errorf("credential bundle field is duplicated")
		}
		var encoded json.RawMessage
		if err := decoder.Decode(&encoded); err != nil {
			return nil, fmt.Errorf("credential bundle value is invalid")
		}
		value, err := decodeStrictCredentialString(encoded)
		if err != nil {
			return nil, err
		}
		values[key] = value
	}
	last, err := decoder.Token()
	if err != nil || last != json.Delim('}') {
		return nil, fmt.Errorf("credential bundle object is incomplete")
	}
	if token, err := decoder.Token(); err != io.EOF || token != nil {
		return nil, fmt.Errorf("credential bundle has trailing data")
	}
	if len(values) != 3 {
		return nil, fmt.Errorf("credential bundle field is missing")
	}
	return values, nil
}

func decodeStrictCredentialString(raw []byte) (string, error) {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' || !utf8.Valid(raw) {
		return "", fmt.Errorf("credential bundle value is not a string")
	}
	for index := 1; index < len(raw)-1; {
		if raw[index] != '\\' {
			_, size := utf8.DecodeRune(raw[index:])
			if size == 0 {
				return "", fmt.Errorf("credential bundle string is invalid")
			}
			index += size
			continue
		}
		index++
		if index >= len(raw)-1 {
			return "", fmt.Errorf("credential bundle escape is invalid")
		}
		if raw[index] != 'u' {
			index++
			continue
		}
		code, ok := decodeJSONHex4(raw, index+1)
		if !ok {
			return "", fmt.Errorf("credential bundle unicode escape is invalid")
		}
		index += 5
		switch {
		case code >= 0xd800 && code <= 0xdbff:
			if index+6 > len(raw)-1 || raw[index] != '\\' || raw[index+1] != 'u' {
				return "", fmt.Errorf("credential bundle contains an unpaired surrogate")
			}
			low, ok := decodeJSONHex4(raw, index+2)
			if !ok || low < 0xdc00 || low > 0xdfff {
				return "", fmt.Errorf("credential bundle contains an unpaired surrogate")
			}
			index += 6
		case code >= 0xdc00 && code <= 0xdfff:
			return "", fmt.Errorf("credential bundle contains an unpaired surrogate")
		}
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("credential bundle string is invalid")
	}
	return value, nil
}

func decodeJSONHex4(raw []byte, start int) (uint16, bool) {
	if start < 0 || start+4 > len(raw) {
		return 0, false
	}
	var value uint16
	for _, item := range raw[start : start+4] {
		value <<= 4
		switch {
		case item >= '0' && item <= '9':
			value += uint16(item - '0')
		case item >= 'a' && item <= 'f':
			value += uint16(item-'a') + 10
		case item >= 'A' && item <= 'F':
			value += uint16(item-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}
