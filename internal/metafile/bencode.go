package metafile

import (
	"bytes"
	"fmt"
	"strconv"
)

type Kind uint8

const (
	KindInteger Kind = iota + 1
	KindBytes
	KindList
	KindDictionary
)

type Node struct {
	Kind  Kind
	Int   int64
	Bytes []byte
	List  []*Node
	Dict  map[string]*Node
	Start int
	End   int
}

func (n *Node) Get(key string) (*Node, bool) {
	if n == nil || n.Kind != KindDictionary {
		return nil, false
	}
	v, ok := n.Dict[key]
	return v, ok
}

type decodeLimits struct {
	maxDepth  int
	maxNodes  int
	maxString int
}

var defaultDecodeLimits = decodeLimits{maxDepth: 64, maxNodes: 1_000_000, maxString: 16 << 20}

type decoder struct {
	data   []byte
	pos    int
	nodes  int
	limits decodeLimits
}

func parseBencode(data []byte) (*Node, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty bencode document")
	}
	if len(data) > 32<<20 {
		return nil, fmt.Errorf("bencode document exceeds 32 MiB")
	}
	d := &decoder{data: data, limits: defaultDecodeLimits}
	node, err := d.parse(0)
	if err != nil {
		return nil, err
	}
	if d.pos != len(data) {
		return nil, fmt.Errorf("trailing data at byte %d", d.pos)
	}
	return node, nil
}

func (d *decoder) parse(depth int) (*Node, error) {
	if depth > d.limits.maxDepth {
		return nil, fmt.Errorf("bencode nesting exceeds %d levels", d.limits.maxDepth)
	}
	if d.pos >= len(d.data) {
		return nil, fmt.Errorf("unexpected end of bencode at byte %d", d.pos)
	}
	d.nodes++
	if d.nodes > d.limits.maxNodes {
		return nil, fmt.Errorf("bencode node count exceeds %d", d.limits.maxNodes)
	}
	start := d.pos
	switch ch := d.data[d.pos]; {
	case ch == 'i':
		d.pos++
		endRel := bytes.IndexByte(d.data[d.pos:], 'e')
		if endRel < 0 {
			return nil, fmt.Errorf("unterminated integer at byte %d", start)
		}
		end := d.pos + endRel
		raw := d.data[d.pos:end]
		if len(raw) == 0 || (len(raw) > 1 && raw[0] == '0') || bytes.Equal(raw, []byte("-0")) || (len(raw) > 2 && raw[0] == '-' && raw[1] == '0') {
			return nil, fmt.Errorf("non-canonical integer at byte %d", start)
		}
		value, err := strconv.ParseInt(string(raw), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid integer at byte %d", start)
		}
		d.pos = end + 1
		return &Node{Kind: KindInteger, Int: value, Start: start, End: d.pos}, nil

	case ch >= '0' && ch <= '9':
		colonRel := bytes.IndexByte(d.data[d.pos:], ':')
		if colonRel < 0 {
			return nil, fmt.Errorf("unterminated byte-string length at byte %d", start)
		}
		colon := d.pos + colonRel
		rawLength := d.data[d.pos:colon]
		if len(rawLength) == 0 || (len(rawLength) > 1 && rawLength[0] == '0') {
			return nil, fmt.Errorf("non-canonical byte-string length at byte %d", start)
		}
		length, err := strconv.ParseInt(string(rawLength), 10, 32)
		if err != nil || length < 0 || length > int64(d.limits.maxString) {
			return nil, fmt.Errorf("invalid or oversized byte string at byte %d", start)
		}
		valueStart := colon + 1
		valueEnd := valueStart + int(length)
		if valueEnd < valueStart || valueEnd > len(d.data) {
			return nil, fmt.Errorf("byte string at byte %d exceeds document", start)
		}
		d.pos = valueEnd
		return &Node{Kind: KindBytes, Bytes: d.data[valueStart:valueEnd], Start: start, End: d.pos}, nil

	case ch == 'l':
		d.pos++
		node := &Node{Kind: KindList, Start: start}
		for {
			if d.pos >= len(d.data) {
				return nil, fmt.Errorf("unterminated list at byte %d", start)
			}
			if d.data[d.pos] == 'e' {
				d.pos++
				node.End = d.pos
				return node, nil
			}
			item, err := d.parse(depth + 1)
			if err != nil {
				return nil, err
			}
			node.List = append(node.List, item)
		}

	case ch == 'd':
		d.pos++
		node := &Node{Kind: KindDictionary, Dict: make(map[string]*Node), Start: start}
		var previous []byte
		for {
			if d.pos >= len(d.data) {
				return nil, fmt.Errorf("unterminated dictionary at byte %d", start)
			}
			if d.data[d.pos] == 'e' {
				d.pos++
				node.End = d.pos
				return node, nil
			}
			key, err := d.parse(depth + 1)
			if err != nil {
				return nil, err
			}
			if key.Kind != KindBytes {
				return nil, fmt.Errorf("dictionary key at byte %d is not a byte string", key.Start)
			}
			if previous != nil && bytes.Compare(previous, key.Bytes) >= 0 {
				return nil, fmt.Errorf("dictionary keys are duplicated or not strictly sorted at byte %d", key.Start)
			}
			previous = append(previous[:0], key.Bytes...)
			value, err := d.parse(depth + 1)
			if err != nil {
				return nil, err
			}
			node.Dict[string(key.Bytes)] = value
		}
	default:
		return nil, fmt.Errorf("invalid bencode token 0x%02x at byte %d", ch, d.pos)
	}
}
