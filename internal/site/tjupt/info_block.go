package tjupt

import (
	"bytes"
	"errors"
	"io"
	"regexp"

	xhtml "golang.org/x/net/html"
)

// The user panel, <table id="info_block">, heads every authenticated TJUPT
// page. Its fields are read only through these labeled patterns; the rest of
// the panel, including the 访问网络 network and location line, is never kept.
var (
	infoUploaded      = regexp.MustCompile(`上传量\s*[:：]\s*([0-9]+(?:\.[0-9]+)?\s*(?:KiB|MiB|GiB|TiB|PiB|KB|MB|GB|TB|PB))\b`)
	infoClass         = regexp.MustCompile(`等级\s*[:：]\s*\[\s*([^\[\]]{1,128}?)\s*\]`)
	infoSeedingPoints = regexp.MustCompile(`做种积分\s*[:：]\s*([0-9][0-9,]*(?:\.[0-9]+)?)(?:\s|$)`)
	infoInvites       = regexp.MustCompile(`邀请\s*[:：]\s*([0-9][0-9,]*)(?:\s|$)`)
	infoSeeding       = regexp.MustCompile(`(?is)<img\b[^>]*\balt\s*=\s*["']Torrents seeding["'][^>]*>\s*([0-9][0-9,]*)`)
	infoLeeching      = regexp.MustCompile(`(?is)<img\b[^>]*\balt\s*=\s*["']Torrents leeching["'][^>]*>\s*([0-9][0-9,]*)`)
)

// infoBlock is the subset of the user panel that ptctl reports. Each field is
// set only when the panel states it unambiguously.
type infoBlock struct {
	uploaded      *int64
	class         string
	seedingPoints string
	invites       *int
	seeding       *int
	leeching      *int
}

func parseInfoBlock(body []byte) infoBlock {
	markup := tableMarkupByID(body, "info_block")
	if markup == nil {
		return infoBlock{}
	}
	text := plainText(string(markup))
	var block infoBlock
	if value, ok := soleSubmatch(infoUploaded, text); ok {
		if size, sized := firstSize([]string{value}); sized {
			block.uploaded = &size
		}
	}
	if value, ok := soleSubmatch(infoClass, text); ok && validAccountText(value, 128) {
		block.class = value
	}
	if value, ok := soleSubmatch(infoSeedingPoints, text); ok {
		block.seedingPoints = canonicalBonusBalance(value)
	}
	if value, ok := soleSubmatch(infoInvites, text); ok {
		block.invites = siteCount(value)
	}
	if value, ok := soleSubmatch(infoSeeding, string(markup)); ok {
		block.seeding = siteCount(value)
	}
	if value, ok := soleSubmatch(infoLeeching, string(markup)); ok {
		block.leeching = siteCount(value)
	}
	return block
}

// soleSubmatch returns the pattern's first group when the text states it at
// least once and every statement agrees.
func soleSubmatch(pattern *regexp.Regexp, text string) (string, bool) {
	value := ""
	for _, match := range pattern.FindAllStringSubmatch(text, 8) {
		if value != "" && match[1] != value {
			return "", false
		}
		value = match[1]
	}
	return value, value != ""
}

// tableMarkupByID returns the markup of the one table whose id attribute is
// id, including the tables nested inside it. A missing or repeated id, or a
// tokenizer error, returns nil.
func tableMarkupByID(body []byte, id string) []byte {
	tokenizer := xhtml.NewTokenizer(bytes.NewReader(body))
	var found []byte
	offset, start, depth := 0, -1, 0
	for {
		tokenType := tokenizer.Next()
		if tokenType == xhtml.ErrorToken {
			if !errors.Is(tokenizer.Err(), io.EOF) || start >= 0 {
				return nil
			}
			return found
		}
		begin := offset
		offset += len(tokenizer.Raw())
		if tokenType != xhtml.StartTagToken && tokenType != xhtml.EndTagToken {
			continue
		}
		name, hasAttributes := tokenizer.TagName()
		if string(name) != "table" {
			continue
		}
		if tokenType == xhtml.EndTagToken {
			if start >= 0 {
				depth--
				if depth == 0 {
					found = body[start:offset]
					start = -1
				}
			}
			continue
		}
		if start >= 0 {
			depth++
			continue
		}
		if hasAttributes && tagHasID(tokenizer, id) {
			if found != nil {
				return nil
			}
			start, depth = begin, 1
		}
	}
}

func tagHasID(tokenizer *xhtml.Tokenizer, id string) bool {
	for {
		key, value, more := tokenizer.TagAttr()
		if string(key) == "id" && string(value) == id {
			return true
		}
		if !more {
			return false
		}
	}
}
