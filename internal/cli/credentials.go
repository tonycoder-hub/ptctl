package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tonycoder-hub/ptctl/internal/downloader"
	"github.com/tonycoder-hub/ptctl/internal/site"
)

func readCredential(reader io.Reader) (site.Credential, error) {
	if err := rejectTTYSecret(reader); err != nil {
		return site.Credential{}, err
	}
	raw, err := io.ReadAll(io.LimitReader(reader, (64<<10)+1))
	if err != nil {
		return site.Credential{}, fmt.Errorf("read session credential from stdin: %w", err)
	}
	if len(raw) > 64<<10 {
		return site.Credential{}, fmt.Errorf("session credential exceeds 64 KiB")
	}
	value := strings.TrimSpace(string(raw))
	return site.NewCookieCredential(value)
}

func readDownloaderCredential(reader io.Reader, username string) (downloader.Credential, error) {
	if err := rejectTTYSecret(reader); err != nil {
		return downloader.Credential{}, err
	}
	raw, err := io.ReadAll(io.LimitReader(reader, (64<<10)+1))
	if err != nil {
		return downloader.Credential{}, fmt.Errorf("read downloader password from stdin: %w", err)
	}
	if len(raw) > 64<<10 {
		return downloader.Credential{}, fmt.Errorf("downloader password exceeds 64 KiB")
	}
	return downloader.NewCredential(username, strings.TrimRight(string(raw), "\r\n"))
}

func rejectTTYSecret(reader io.Reader) error {
	file, ok := reader.(*os.File)
	if !ok {
		return nil
	}
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect secret input: %w", err)
	}
	if info.Mode()&os.ModeCharDevice != 0 {
		return fmt.Errorf("refusing to read a secret from an interactive terminal; pipe it to the --*-stdin command")
	}
	return nil
}
