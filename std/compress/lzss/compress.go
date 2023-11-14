package lzss

import (
	"bytes"
	"fmt"
	"math/bits"

	"github.com/consensys/gnark/std/compress/lzss/internal/suffixarray"
	"github.com/icza/bitio"
)

type Compressor struct {
	buf bytes.Buffer
	bw  *bitio.Writer

	inputIndex *suffixarray.Index
	inputSa    [maxInputSize]int32 // suffix array space.

	dictData  []byte
	dictIndex *suffixarray.Index
	dictSa    [maxDictSize]int32 // suffix array space.

	compressionMode CompressionMode
}

type CompressionMode uint8

const (
	BestCompression        CompressionMode = 1 // BestCompression allows the compressor to produce a stream of bit-level granularity, Giving the compressor this freedom helps it achieve better compression ratios but will impose a high number of constraints on the SNARK decompressor
	GoodCompression        CompressionMode = 2
	GoodSnarkDecompression CompressionMode = 4
	BestSnarkDecompression CompressionMode = 8 // BestSnarkDecomposition forces the compressor to produce byte-aligned output. It is convenient and efficient for the SNARK decompressor but can hurt the compression ratio significantly =
)

// NewCompressor returns a new compressor with the given dictionary
func NewCompressor(dict []byte, compressionMode CompressionMode) (*Compressor, error) {
	dict = augmentDict(dict)
	if len(dict) > maxDictSize {
		return nil, fmt.Errorf("dict size must be <= %d", maxDictSize)
	}
	c := &Compressor{
		dictData: dict,
	}
	c.buf.Grow(maxInputSize)
	c.dictIndex = suffixarray.New(c.dictData, c.dictSa[:len(c.dictData)])
	c.compressionMode = compressionMode
	return c, nil
}

func augmentDict(dict []byte) []byte {
	return append(dict, symbolDict, symbolShort, symbolLong)
}

func initBackRefTypes(dictLen int, compressionMode CompressionMode) (short, long, dict backrefType) {
	wordAlign := func(a int) uint8 {
		return (uint8(a) + uint8(compressionMode) - 1) / uint8(compressionMode) * uint8(compressionMode)
	}
	short = newBackRefType(symbolShort, wordAlign(14), 8, false)
	long = newBackRefType(symbolLong, wordAlign(19), 8, false)
	dict = newBackRefType(symbolDict, wordAlign(bits.Len(uint(dictLen))), 8, true)
	return
}

// Compress compresses the given data
func (compressor *Compressor) Compress(d []byte) (c []byte, err error) {
	// check input size
	if len(d) > maxInputSize {
		return nil, fmt.Errorf("input size must be <= %d", maxInputSize)
	}

	// reset output buffer
	compressor.buf.Reset()
	compressor.bw = bitio.NewWriter(&compressor.buf)

	// build the index
	compressor.inputIndex = suffixarray.New(d, compressor.inputSa[:len(d)])

	shortBackRefType, longBackRefType, dictBackRefType := initBackRefTypes(len(compressor.dictData), compressor.compressionMode)

	bDict := backref{bType: dictBackRefType, length: -1, address: -1}
	bShort := backref{bType: shortBackRefType, length: -1, address: -1}
	bLong := backref{bType: longBackRefType, length: -1, address: -1}

	fillBackrefs := func(i int, minLen int) bool {
		bDict.address, bDict.length = compressor.findBackRef(d, i, dictBackRefType, minLen)
		bShort.address, bShort.length = compressor.findBackRef(d, i, shortBackRefType, minLen)
		bLong.address, bLong.length = compressor.findBackRef(d, i, longBackRefType, minLen)
		return !(bDict.length == -1 && bShort.length == -1 && bLong.length == -1)
	}
	bestBackref := func() (backref, int) {
		if bDict.length != -1 && bDict.savings() > bShort.savings() && bDict.savings() > bLong.savings() {
			return bDict, bDict.savings()
		}
		if bShort.length != -1 && bShort.savings() > bLong.savings() {
			return bShort, bShort.savings()
		}
		return bLong, bLong.savings()
	}

	for i := 0; i < len(d); {
		if !canEncodeSymbol(d[i]) {
			// we must find a backref.
			if !fillBackrefs(i, 1) {
				// we didn't find a backref but can't write the symbol directly
				return nil, fmt.Errorf("could not find a backref at index %d", i)
			}
			best, _ := bestBackref()
			best.writeTo(compressor.bw, i)
			i += best.length
			continue
		}
		if !fillBackrefs(i, -1) {
			// we didn't find a backref, let's write the symbol directly
			compressor.writeByte(d[i])
			i++
			continue
		}
		bestAtI, bestSavings := bestBackref()

		if i+1 < len(d) {
			if fillBackrefs(i+1, bestAtI.length+1) {
				if newBest, newSavings := bestBackref(); newSavings > bestSavings {
					// we found an even better backref
					compressor.writeByte(d[i])
					i++

					// then emit the backref at i+1
					bestSavings = newSavings
					bestAtI = newBest

					// can we find an even better backref?
					if canEncodeSymbol(d[i]) && i+1 < len(d) {
						if fillBackrefs(i+1, bestAtI.length+1) {
							// we found an even better backref
							if newBest, newSavings := bestBackref(); newSavings > bestSavings {
								compressor.writeByte(d[i])
								i++

								// bestSavings = newSavings
								bestAtI = newBest
							}
						}
					}
				}
			} else if i+2 < len(d) && canEncodeSymbol(d[i+1]) {
				// maybe at i+2 ? (we already tried i+1)
				if fillBackrefs(i+2, bestAtI.length+2) {
					if newBest, newSavings := bestBackref(); newSavings > bestSavings {
						// we found a better backref
						// write the symbol at i
						compressor.writeByte(d[i])
						i++
						compressor.writeByte(d[i])
						i++

						// then emit the backref at i+2
						bestAtI = newBest
						// bestSavings = newSavings
					}
				}
			}
		}

		bestAtI.writeTo(compressor.bw, i)
		i += bestAtI.length
	}

	if compressor.bw.TryError != nil {
		return nil, compressor.bw.TryError
	}
	if err := compressor.bw.Close(); err != nil {
		return nil, err
	}

	return compressor.buf.Bytes(), nil
}

// canEncodeSymbol returns true if the symbol can be encoded directly
func canEncodeSymbol(b byte) bool {
	return b != symbolDict && b != symbolShort && b != symbolLong
}

func (compressor *Compressor) writeByte(b byte) {
	if !canEncodeSymbol(b) {
		panic("cannot encode symbol")
	}
	compressor.bw.TryWriteByte(b)
}

// findBackRef attempts to find a backref in the window [i-brAddressRange, i+brLengthRange]
// if no backref is found, it returns -1, -1
// else returns the address and length of the backref
func (compressor *Compressor) findBackRef(data []byte, i int, bType backrefType, minLength int) (addr, length int) {
	if minLength == -1 {
		minLength = bType.nbBytesBackRef
	}

	if i+minLength > len(data) {
		return -1, -1
	}

	windowStart := max(0, i-bType.maxAddress)
	maxRefLen := bType.maxLength

	if i+maxRefLen > len(data) {
		maxRefLen = len(data) - i
	}

	if minLength > maxRefLen {
		return -1, -1
	}

	if bType.dictOnly {
		return compressor.dictIndex.LookupLongest(data[i:i+maxRefLen], minLength, maxRefLen, 0, len(compressor.dictData))
	}

	return compressor.inputIndex.LookupLongest(data[i:i+maxRefLen], minLength, maxRefLen, windowStart, i)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
