package transmission

import (
	"context"
	"testing"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
)

func FuzzDecodeRPCResult(f *testing.F) {
	f.Add([]byte(`{"jsonrpc":"2.0","result":{},"id":1}`), true)
	f.Add([]byte(`{"arguments":{},"result":"success","tag":1}`), false)
	f.Add([]byte(`{"jsonrpc":"2.0","result":{"x":"\ud800"},"id":1}`), true)
	f.Fuzz(func(t *testing.T, body []byte, modern bool) {
		valueProtocol := protocolLegacy
		if modern {
			valueProtocol = protocolJSONRPC
		}
		_, _ = decodeRPCResult(body, valueProtocol, 1)
	})
}

func FuzzDecodeFileResult(f *testing.F) {
	f.Add([]byte(`{"torrents":[]}`), true)
	f.Add([]byte(`{"torrents":[{"hashString":"0123456789abcdef0123456789abcdef01234567","name":"bundle","downloadDir":"/downloads","files":[],"fileStats":[]}]}`), false)
	f.Fuzz(func(t *testing.T, body []byte, modern bool) {
		valueProtocol := protocolLegacy
		if modern {
			valueProtocol = protocolJSONRPC
		}
		limits := downloader.DefaultJobFileLedgerLimits()
		limits.MaxFiles = 32
		limits.MaxPathBytes = 4 << 10
		limits.MaxResponseBytes = 64 << 10
		_, _, _, _, _ = decodeFileResult(context.Background(), body, valueProtocol, testHash, limits)
	})
}
