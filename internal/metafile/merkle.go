package metafile

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
)

const v2BlockSize int64 = 16 << 10

type v2Hash [sha256.Size]byte

type merkleAccumulator struct {
	hashes [64]v2Hash
	used   [64]bool
}

func (a *merkleAccumulator) add(hash v2Hash, level uint) error {
	for {
		if level >= uint(len(a.hashes)) {
			return fmt.Errorf("v2 Merkle tree exceeds supported height")
		}
		if !a.used[level] {
			a.hashes[level] = hash
			a.used[level] = true
			return nil
		}
		hash = v2HashPair(a.hashes[level], hash)
		a.used[level] = false
		level++
	}
}

func (a *merkleAccumulator) root() (v2Hash, error) {
	var root v2Hash
	found := false
	for level := range a.used {
		if !a.used[level] {
			continue
		}
		if found {
			return v2Hash{}, fmt.Errorf("v2 Merkle tree is not complete")
		}
		root = a.hashes[level]
		found = true
	}
	if !found {
		return v2Hash{}, fmt.Errorf("v2 Merkle tree has no leaves")
	}
	return root, nil
}

func v2HashPair(left, right v2Hash) v2Hash {
	var pair [sha256.Size * 2]byte
	copy(pair[:sha256.Size], left[:])
	copy(pair[sha256.Size:], right[:])
	return sha256.Sum256(pair[:])
}

// v2ZeroHash returns the root of a complete all-padding subtree at level.
// BEP 52 defines an unused 16 KiB leaf hash as 32 zero bytes, not SHA-256 of
// an empty block.
func v2ZeroHash(level uint) v2Hash {
	var zero v2Hash
	for current := uint(0); current < level; current++ {
		zero = v2HashPair(zero, zero)
	}
	return zero
}

func v2PieceLayerDepth(pieceLength int64) (uint, error) {
	if pieceLength < v2BlockSize || pieceLength&(pieceLength-1) != 0 || pieceLength%v2BlockSize != 0 {
		return 0, fmt.Errorf("invalid v2 piece length")
	}
	blocks := pieceLength / v2BlockSize
	var depth uint
	for blocks > 1 {
		blocks >>= 1
		depth++
	}
	return depth, nil
}

func nextPowerOfTwo(value int64) (int64, error) {
	if value <= 0 {
		return 0, fmt.Errorf("v2 Merkle leaf count must be positive")
	}
	power := int64(1)
	for power < value {
		if power > int64(^uint64(0)>>1)/2 {
			return 0, fmt.Errorf("v2 Merkle leaf count overflows int64")
		}
		power *= 2
	}
	return power, nil
}

// reduceV2PieceLayer authenticates a BEP 52 piece layer against its file root
// without allocating a second slice of hashes. Each supplied hash is already
// at the layer selected by piece length.
func reduceV2PieceLayer(layer []byte, count int64, pieceLength int64) (v2Hash, error) {
	depth, err := v2PieceLayerDepth(pieceLength)
	if err != nil {
		return v2Hash{}, err
	}
	if count <= 0 || count > int64(^uint64(0)>>1)/sha256.Size || int64(len(layer)) != count*sha256.Size {
		return v2Hash{}, fmt.Errorf("invalid v2 piece layer length")
	}
	target, err := nextPowerOfTwo(count)
	if err != nil {
		return v2Hash{}, err
	}
	var accumulator merkleAccumulator
	for index := int64(0); index < count; index++ {
		start := int(index * sha256.Size)
		var hash v2Hash
		copy(hash[:], layer[start:start+sha256.Size])
		if err := accumulator.add(hash, depth); err != nil {
			return v2Hash{}, err
		}
	}
	zero := v2ZeroHash(depth)
	for index := count; index < target; index++ {
		if err := accumulator.add(zero, depth); err != nil {
			return v2Hash{}, err
		}
	}
	return accumulator.root()
}

// hashV2Segment hashes one file segment. targetLeaves is the complete power-
// of-two block subtree represented by the result. A short final data block is
// hashed at its actual length; only leaves wholly beyond EOF are zero hashes.
func hashV2Segment(ctx context.Context, reader io.Reader, dataBytes, targetLeaves int64, buffer []byte) (v2Hash, error) {
	if dataBytes <= 0 {
		return v2Hash{}, fmt.Errorf("v2 Merkle segment must contain data")
	}
	if targetLeaves <= 0 || targetLeaves&(targetLeaves-1) != 0 {
		return v2Hash{}, fmt.Errorf("v2 Merkle target leaf count is not a power of two")
	}
	actualLeaves := ((dataBytes - 1) / v2BlockSize) + 1
	if actualLeaves > targetLeaves || len(buffer) < int(v2BlockSize) {
		return v2Hash{}, fmt.Errorf("invalid v2 Merkle segment bounds")
	}
	var accumulator merkleAccumulator
	remaining := dataBytes
	for leaf := int64(0); leaf < actualLeaves; leaf++ {
		select {
		case <-ctx.Done():
			return v2Hash{}, ctx.Err()
		default:
		}
		blockBytes := v2BlockSize
		if remaining < blockBytes {
			blockBytes = remaining
		}
		if _, err := io.ReadFull(reader, buffer[:int(blockBytes)]); err != nil {
			return v2Hash{}, err
		}
		hash := sha256.Sum256(buffer[:int(blockBytes)])
		if err := accumulator.add(hash, 0); err != nil {
			return v2Hash{}, err
		}
		remaining -= blockBytes
	}
	for leaf := actualLeaves; leaf < targetLeaves; leaf++ {
		if err := accumulator.add(v2Hash{}, 0); err != nil {
			return v2Hash{}, err
		}
	}
	return accumulator.root()
}
