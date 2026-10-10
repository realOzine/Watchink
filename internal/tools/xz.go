package tools

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"io"
)

// This file is a small, read-only decoder for the .xz container with the
// LZMA2 filter, which is all `xz` and `tar -J` produce by default and what
// the Linux FFmpeg builds ship as. The standard library has no xz
// support and the project takes no dependency for it. Not supported,
// and reported as errors: BCJ / delta filter chains.
//
// Format references: the .xz file format specification 1.1.0 and the LZMA
// SDK's lzma-specification.txt.

var errXZ = errors.New("xz: corrupt input")

func xzErr(what string) error { return fmt.Errorf("%w: %s", errXZ, what) }

const (
	xzHeaderSize = 12
	xzFilterLZMA = 0x21

	xzCheckNone   = 0x0
	xzCheckCRC32  = 0x1
	xzCheckCRC64  = 0x4
	xzCheckSHA256 = 0xA

	// xzMaxDict bounds the window a file may ask for; `xz -9` uses 64 MiB.
	xzMaxDict = 512 << 20
)

var (
	xzMagic       = []byte{0xFD, '7', 'z', 'X', 'Z', 0x00}
	xzFooterMagic = []byte{'Y', 'Z'}
	crc64ECMA     = crc64.MakeTable(crc64.ECMA)
)

// xzCheckSize returns the size of the integrity check with the given ID
// (file format §2.1.1.2); IDs this decoder cannot verify are still skipped
// correctly.
func xzCheckSize(id byte) int {
	switch {
	case id == 0:
		return 0
	case id <= 3:
		return 4
	case id <= 6:
		return 8
	case id <= 9:
		return 16
	case id <= 12:
		return 32
	default:
		return 64
	}
}

// countingReader tracks how many bytes of the container were consumed, which
// the block padding and the index verification need.
type countingReader struct {
	r *bufio.Reader
	n int64
}

func (c *countingReader) ReadByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.n++
	}
	return b, err
}

func (c *countingReader) full(p []byte) error {
	n, err := io.ReadFull(c.r, p)
	c.n += int64(n)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return xzErr("unexpected end of file")
	}
	return err
}

func (c *countingReader) byteOrErr() (byte, error) {
	b, err := c.ReadByte()
	if errors.Is(err, io.EOF) {
		return 0, xzErr("unexpected end of file")
	}
	return b, err
}

// xzVLI reads a variable-length integer (file format §1.2).
func xzVLI(r io.ByteReader) (uint64, error) {
	var v uint64
	for i := 0; i < 9; i++ {
		b, err := r.ReadByte()
		if errors.Is(err, io.EOF) {
			return 0, xzErr("unexpected end of file")
		}
		if err != nil {
			return 0, err
		}
		v |= uint64(b&0x7F) << (7 * i)
		if b&0x80 == 0 {
			if b == 0 && i > 0 {
				return 0, xzErr("non-minimal integer")
			}
			return v, nil
		}
	}
	return 0, xzErr("integer too long")
}

// xzBlockRecord is what the index must repeat about a block.
type xzBlockRecord struct{ unpadded, uncompressed uint64 }

// xzReader decodes one or more concatenated .xz streams.
type xzReader struct {
	in  countingReader
	err error

	// Per stream.
	inStream bool
	check    byte
	records  []xzBlockRecord

	// Per block.
	inBlock      bool
	blockStart   int64 // container offset of the block header
	headerSize   int64
	wantComp     int64 // -1 when the header does not say
	wantUncomp   int64
	uncompressed int64
	sum          hash.Hash
	lz           *lzma2Decoder
	chunkDone    bool // the LZMA2 end marker was read

	pending []byte // decoded bytes not yet returned by Read
}

// newXZReader returns a reader of the decompressed content of r.
func newXZReader(r io.Reader) io.Reader {
	return &xzReader{in: countingReader{r: bufio.NewReaderSize(r, 256<<10)}}
}

func (z *xzReader) Read(p []byte) (int, error) {
	for len(z.pending) == 0 {
		if z.err != nil {
			return 0, z.err
		}
		if z.err = z.advance(); z.err != nil {
			return 0, z.err
		}
	}
	n := copy(p, z.pending)
	z.pending = z.pending[n:]
	return n, nil
}

// advance makes progress by one container element or one LZMA2 chunk. It
// returns io.EOF after the last stream.
func (z *xzReader) advance() error {
	switch {
	case z.inBlock:
		return z.readChunk()
	case z.inStream:
		return z.blockOrIndex()
	default:
		return z.streamHeader()
	}
}

func (z *xzReader) streamHeader() error {
	// Stream padding (file format §2.2): zero bytes in multiples of four.
	padding := 0
	for {
		b, err := z.in.r.Peek(1)
		if errors.Is(err, io.EOF) {
			if len(z.records) == 0 && z.in.n == 0 {
				return xzErr("empty file")
			}
			if padding%4 != 0 {
				return xzErr("bad stream padding")
			}
			return io.EOF
		}
		if err != nil {
			return err
		}
		if b[0] != 0 {
			break
		}
		_, _ = z.in.ReadByte()
		padding++
	}
	if padding%4 != 0 {
		return xzErr("bad stream padding")
	}
	var hdr [xzHeaderSize]byte
	if err := z.in.full(hdr[:]); err != nil {
		return err
	}
	if !bytes.Equal(hdr[:6], xzMagic) {
		return xzErr("not an xz file")
	}
	if crc32.ChecksumIEEE(hdr[6:8]) != binary.LittleEndian.Uint32(hdr[8:]) {
		return xzErr("stream header checksum")
	}
	if hdr[6] != 0 || hdr[7]&0xF0 != 0 {
		return xzErr("unsupported stream flags")
	}
	z.check = hdr[7]
	z.records = z.records[:0]
	z.inStream = true
	return nil
}

func (z *xzReader) blockOrIndex() error {
	start := z.in.n
	first, err := z.in.byteOrErr()
	if err != nil {
		return err
	}
	if first == 0 {
		return z.indexAndFooter(start)
	}

	// Block header (file format §3.1).
	hdr := make([]byte, (int(first)+1)*4)
	hdr[0] = first
	if err := z.in.full(hdr[1:]); err != nil {
		return err
	}
	body, sum := hdr[:len(hdr)-4], binary.LittleEndian.Uint32(hdr[len(hdr)-4:])
	if crc32.ChecksumIEEE(body) != sum {
		return xzErr("block header checksum")
	}
	f := bytes.NewReader(body[1:])
	flags, _ := f.ReadByte() // a header is at least 8 bytes long
	if flags&0x3C != 0 {
		return xzErr("unsupported block flags")
	}
	if flags&0x03 != 0 {
		return fmt.Errorf("xz: unsupported filter chain (only LZMA2 is implemented)")
	}
	z.wantComp, z.wantUncomp = -1, -1
	if flags&0x40 != 0 {
		v, err := xzVLI(f)
		if err != nil || v == 0 || v > 1<<62 {
			return xzErr("block header")
		}
		z.wantComp = int64(v)
	}
	if flags&0x80 != 0 {
		v, err := xzVLI(f)
		if err != nil || v > 1<<62 {
			return xzErr("block header")
		}
		z.wantUncomp = int64(v)
	}
	id, err := xzVLI(f)
	if err != nil {
		return xzErr("block header")
	}
	if id != xzFilterLZMA {
		return fmt.Errorf("xz: unsupported filter %#x (only LZMA2 is implemented)", id)
	}
	propSize, err := xzVLI(f)
	if err != nil || propSize != 1 {
		return xzErr("LZMA2 properties")
	}
	dictByte, err := f.ReadByte()
	if err != nil || dictByte > 40 {
		return xzErr("LZMA2 properties")
	}
	for {
		b, err := f.ReadByte()
		if err != nil {
			break
		}
		if b != 0 {
			return xzErr("block header padding")
		}
	}
	dict := uint64(1<<32 - 1)
	if dictByte < 40 {
		dict = uint64(2|dictByte&1) << (dictByte/2 + 11)
	}
	if dict > xzMaxDict {
		return fmt.Errorf("xz: dictionary of %d MiB is too large", dict>>20)
	}

	z.blockStart, z.headerSize = start, int64(len(hdr))
	z.uncompressed = 0
	z.chunkDone = false
	switch z.check {
	case xzCheckNone:
		z.sum = nil
	case xzCheckCRC32:
		z.sum = crc32.NewIEEE()
	case xzCheckCRC64:
		z.sum = crc64.New(crc64ECMA)
	case xzCheckSHA256:
		z.sum = sha256.New()
	default:
		z.sum = nil // unknown check: skipped, the payload was verified as a whole
	}
	if z.lz == nil {
		z.lz = &lzma2Decoder{}
	}
	z.lz.start(int(dict))
	z.inBlock = true
	return nil
}

// readChunk decodes the next LZMA2 chunk of the current block, or finishes
// the block at the end marker.
func (z *xzReader) readChunk() error {
	out, done, err := z.lz.chunk(&z.in)
	if err != nil {
		return err
	}
	if !done {
		z.uncompressed += int64(len(out))
		if z.wantUncomp >= 0 && z.uncompressed > z.wantUncomp {
			return xzErr("block is larger than its header says")
		}
		if z.sum != nil {
			z.sum.Write(out)
		}
		z.pending = out
		return nil
	}

	// Block padding and check (file format §3.3).
	comp := z.in.n - z.blockStart - z.headerSize
	if (z.wantComp >= 0 && comp != z.wantComp) || (z.wantUncomp >= 0 && z.uncompressed != z.wantUncomp) {
		return xzErr("block size differs from its header")
	}
	for i := comp; i%4 != 0; i++ {
		b, err := z.in.byteOrErr()
		if err != nil {
			return err
		}
		if b != 0 {
			return xzErr("block padding")
		}
	}
	check := make([]byte, xzCheckSize(z.check))
	if err := z.in.full(check); err != nil {
		return err
	}
	if z.sum != nil {
		got := z.sum.Sum(nil)
		if z.check != xzCheckSHA256 {
			// The CRCs are stored little-endian; hash.Hash yields big-endian.
			for i, j := 0, len(got)-1; i < j; i, j = i+1, j-1 {
				got[i], got[j] = got[j], got[i]
			}
		}
		if !bytes.Equal(got, check) {
			return xzErr("block checksum mismatch")
		}
	}
	z.records = append(z.records, xzBlockRecord{
		unpadded:     uint64(z.headerSize + comp + int64(len(check))),
		uncompressed: uint64(z.uncompressed),
	})
	z.inBlock = false
	return nil
}

// indexAndFooter verifies the index against the blocks that were decoded and
// reads the stream footer (file format §4). start is the container
// offset of the index indicator, which has already been consumed.
func (z *xzReader) indexAndFooter(start int64) error {
	// The index is checksummed as raw bytes, indicator included.
	tee := &crcByteReader{c: &z.in, crc: crc32.NewIEEE()}
	tee.crc.Write([]byte{0})
	n, err := xzVLI(tee)
	if err != nil {
		return err
	}
	if n != uint64(len(z.records)) {
		return xzErr("index does not match the blocks")
	}
	for _, rec := range z.records {
		unpadded, err := xzVLI(tee)
		if err != nil {
			return err
		}
		uncompressed, err := xzVLI(tee)
		if err != nil {
			return err
		}
		if unpadded != rec.unpadded || uncompressed != rec.uncompressed {
			return xzErr("index does not match the blocks")
		}
	}
	for (z.in.n-start)%4 != 0 {
		b, err := tee.ReadByte()
		if err != nil {
			return err
		}
		if b != 0 {
			return xzErr("index padding")
		}
	}
	indexSize := z.in.n - start + 4
	var tail [4 + xzHeaderSize]byte
	if err := z.in.full(tail[:]); err != nil {
		return err
	}
	if binary.LittleEndian.Uint32(tail[:4]) != tee.crc.Sum32() {
		return xzErr("index checksum")
	}
	footer := tail[4:]
	if !bytes.Equal(footer[10:], xzFooterMagic) ||
		crc32.ChecksumIEEE(footer[4:10]) != binary.LittleEndian.Uint32(footer[:4]) {
		return xzErr("stream footer")
	}
	if backward := (int64(binary.LittleEndian.Uint32(footer[4:8])) + 1) * 4; backward != indexSize {
		return xzErr("stream footer does not match the index")
	}
	if footer[8] != 0 || footer[9] != z.check {
		return xzErr("stream footer does not match the header")
	}
	z.inStream = false
	return nil
}

// crcByteReader feeds every byte it reads into a CRC32.
type crcByteReader struct {
	c   *countingReader
	crc hash.Hash32
}

func (t *crcByteReader) ReadByte() (byte, error) {
	b, err := t.c.byteOrErr()
	if err == nil {
		t.crc.Write([]byte{b})
	}
	return b, err
}

// --- LZMA2 -----------------------------------------------------------------

const (
	lzmaStates       = 12
	lzmaPosStatesMax = 1 << 4
	lzmaMatchMinLen  = 2
	lzmaLenToPos     = 4
	lzmaStartPosIdx  = 4
	lzmaEndPosIdx    = 14
	lzmaFullDist     = 1 << (lzmaEndPosIdx / 2)
	lzmaAlignBits    = 4
	lzmaProbInit     = 1 << 10
	lzmaTopValue     = 1 << 24

	// lzma2MaxChunk is the largest uncompressed size of one LZMA2 chunk.
	lzma2MaxChunk = 1 << 21
)

type lzmaLenDecoder struct {
	choice, choice2 uint16
	low, mid        [lzmaPosStatesMax][8]uint16
	high            [256]uint16
}

func (l *lzmaLenDecoder) reset() {
	l.choice, l.choice2 = lzmaProbInit, lzmaProbInit
	for i := range l.low {
		fillProbs(l.low[i][:])
		fillProbs(l.mid[i][:])
	}
	fillProbs(l.high[:])
}

func fillProbs(p []uint16) {
	for i := range p {
		p[i] = lzmaProbInit
	}
}

// lzma2Decoder decodes the chunks of one LZMA2 stream. The dictionary is the
// tail of win: decoded bytes are appended to it and the front is dropped once
// it has grown well past the dictionary size.
type lzma2Decoder struct {
	win      []byte
	dictSize int
	pos      uint64 // bytes produced since the last dictionary reset

	needDictReset, needProps bool

	lc, lp, pb uint
	state      uint32
	rep        [4]uint32

	isMatch    [lzmaStates][lzmaPosStatesMax]uint16
	isRep      [lzmaStates]uint16
	isRepG0    [lzmaStates]uint16
	isRepG1    [lzmaStates]uint16
	isRepG2    [lzmaStates]uint16
	isRep0Long [lzmaStates][lzmaPosStatesMax]uint16
	posSlot    [lzmaLenToPos][64]uint16
	posSpecial [1 + lzmaFullDist - lzmaEndPosIdx]uint16
	posAlign   [1 << lzmaAlignBits]uint16
	lenDec     lzmaLenDecoder
	repLenDec  lzmaLenDecoder
	literal    []uint16

	// Range decoder over the compressed bytes of the current chunk.
	rng, code uint32
	in        []byte
	inPos     int
	overrun   bool

	raw []byte // reused buffer for the compressed chunk
}

// start prepares the decoder for a new block.
func (d *lzma2Decoder) start(dictSize int) {
	if dictSize < 4096 {
		dictSize = 4096
	}
	d.dictSize = dictSize
	d.win = d.win[:0]
	d.pos = 0
	d.needDictReset, d.needProps = true, true
}

func (d *lzma2Decoder) resetState() {
	d.state = 0
	d.rep = [4]uint32{}
	for i := range d.isMatch {
		fillProbs(d.isMatch[i][:])
		fillProbs(d.isRep0Long[i][:])
	}
	fillProbs(d.isRep[:])
	fillProbs(d.isRepG0[:])
	fillProbs(d.isRepG1[:])
	fillProbs(d.isRepG2[:])
	for i := range d.posSlot {
		fillProbs(d.posSlot[i][:])
	}
	fillProbs(d.posSpecial[:])
	fillProbs(d.posAlign[:])
	d.lenDec.reset()
	d.repLenDec.reset()
	fillProbs(d.literal)
}

func (d *lzma2Decoder) setProps(b byte) error {
	if b >= 9*5*5 {
		return xzErr("LZMA properties")
	}
	d.lc = uint(b % 9)
	b /= 9
	d.lp, d.pb = uint(b%5), uint(b/5)
	if d.lc+d.lp > 4 {
		return xzErr("LZMA properties")
	}
	if n := 0x300 << (d.lc + d.lp); len(d.literal) != n {
		d.literal = make([]uint16, n)
	}
	return nil
}

// chunk reads and decodes the next chunk. The returned slice aliases the
// window and is only valid until the next call. done reports the end marker.
func (d *lzma2Decoder) chunk(in *countingReader) (out []byte, done bool, err error) {
	control, err := in.byteOrErr()
	if err != nil {
		return nil, false, err
	}
	if control == 0 {
		return nil, true, nil
	}
	if control >= 0xE0 || control == 1 {
		d.needProps = true
		d.needDictReset = false
		d.win = d.win[:0]
		d.pos = 0
	} else if d.needDictReset {
		return nil, false, xzErr("LZMA2 stream does not start with a dictionary reset")
	}

	// Drop the part of the window no match can reach any more.
	if slack := max(d.dictSize/2, 2*lzma2MaxChunk); len(d.win) > d.dictSize+slack {
		n := copy(d.win, d.win[len(d.win)-d.dictSize:])
		d.win = d.win[:n]
	}
	start := len(d.win)

	var hdr [4]byte
	if control < 0x80 {
		if control > 2 {
			return nil, false, xzErr("LZMA2 control byte")
		}
		if err := in.full(hdr[:2]); err != nil {
			return nil, false, err
		}
		size := int(binary.BigEndian.Uint16(hdr[:2])) + 1
		d.win = append(d.win, make([]byte, size)...)
		if err := in.full(d.win[start:]); err != nil {
			return nil, false, err
		}
		d.pos += uint64(size)
		return d.win[start:], false, nil
	}

	if err := in.full(hdr[:]); err != nil {
		return nil, false, err
	}
	size := (int(control&0x1F)<<16 | int(binary.BigEndian.Uint16(hdr[:2]))) + 1
	compSize := int(binary.BigEndian.Uint16(hdr[2:])) + 1
	switch {
	case control >= 0xC0:
		props, err := in.byteOrErr()
		if err != nil {
			return nil, false, err
		}
		if err := d.setProps(props); err != nil {
			return nil, false, err
		}
		d.needProps = false
		d.resetState()
	case d.needProps:
		return nil, false, xzErr("LZMA2 chunk without properties")
	case control >= 0xA0:
		d.resetState()
	}

	if cap(d.raw) < compSize {
		d.raw = make([]byte, 1<<16)
	}
	d.in, d.inPos, d.overrun = d.raw[:compSize], 0, false
	if err := in.full(d.in); err != nil {
		return nil, false, err
	}
	if compSize < 5 || d.in[0] != 0 {
		return nil, false, xzErr("range coder header")
	}
	d.code = binary.BigEndian.Uint32(d.in[1:5])
	d.rng = 0xFFFFFFFF
	d.inPos = 5

	if err := d.decode(size); err != nil {
		return nil, false, err
	}
	if d.overrun || d.inPos != len(d.in) || d.code != 0 {
		return nil, false, xzErr("LZMA chunk size mismatch")
	}
	return d.win[start:], false, nil
}

func (d *lzma2Decoder) normalize() {
	if d.rng < lzmaTopValue {
		d.rng <<= 8
		var b byte
		if d.inPos < len(d.in) {
			b = d.in[d.inPos]
			d.inPos++
		} else {
			d.overrun = true
		}
		d.code = d.code<<8 | uint32(b)
	}
}

func (d *lzma2Decoder) bit(p *uint16) uint32 {
	bound := (d.rng >> 11) * uint32(*p)
	if d.code < bound {
		d.rng = bound
		*p += (1<<11 - *p) >> 5
		d.normalize()
		return 0
	}
	d.rng -= bound
	d.code -= bound
	*p -= *p >> 5
	d.normalize()
	return 1
}

func (d *lzma2Decoder) direct(n uint32) uint32 {
	var v uint32
	for ; n > 0; n-- {
		d.rng >>= 1
		d.code -= d.rng
		t := 0 - (d.code >> 31)
		d.code += d.rng & t
		d.normalize()
		v = v<<1 + t + 1
	}
	return v
}

func (d *lzma2Decoder) tree(probs []uint16, bits uint) uint32 {
	m := uint32(1)
	for i := uint(0); i < bits; i++ {
		m = m<<1 | d.bit(&probs[m])
	}
	return m - 1<<bits
}

func (d *lzma2Decoder) reverseTree(probs []uint16, bits uint) uint32 {
	m, v := uint32(1), uint32(0)
	for i := uint(0); i < bits; i++ {
		b := d.bit(&probs[m])
		m = m<<1 | b
		v |= b << i
	}
	return v
}

func (d *lzma2Decoder) length(l *lzmaLenDecoder, posState uint32) uint32 {
	if d.bit(&l.choice) == 0 {
		return d.tree(l.low[posState][:], 3)
	}
	if d.bit(&l.choice2) == 0 {
		return 8 + d.tree(l.mid[posState][:], 3)
	}
	return 16 + d.tree(l.high[:], 8)
}

// decode appends exactly n bytes to the window from the current LZMA chunk.
func (d *lzma2Decoder) decode(n int) error {
	end := len(d.win) + n
	posMask := uint64(1)<<d.pb - 1
	litPosMask := uint64(1)<<d.lp - 1
	for len(d.win) < end {
		if d.overrun {
			return xzErr("LZMA chunk is truncated")
		}
		posState := uint32(d.pos & posMask)
		state := d.state

		if d.bit(&d.isMatch[state][posState]) == 0 {
			var prev byte
			if d.pos > 0 {
				prev = d.win[len(d.win)-1]
			}
			lit := uint32(d.pos&litPosMask)<<d.lc + uint32(prev)>>(8-d.lc)
			probs := d.literal[0x300*lit : 0x300*(lit+1)]
			sym := uint32(1)
			if state >= 7 {
				// After a match the byte at the last distance predicts this one.
				if uint64(d.rep[0]) >= min(d.pos, uint64(len(d.win))) {
					return xzErr("LZMA match distance is out of range")
				}
				match := uint32(d.win[len(d.win)-int(d.rep[0])-1])
				for sym < 0x100 {
					mb := (match >> 7) & 1
					match <<= 1
					b := d.bit(&probs[(1+mb)<<8+sym])
					sym = sym<<1 | b
					if mb != b {
						break
					}
				}
			}
			for sym < 0x100 {
				sym = sym<<1 | d.bit(&probs[sym])
			}
			d.win = append(d.win, byte(sym))
			d.pos++
			switch {
			case state < 4:
				d.state = 0
			case state < 10:
				d.state = state - 3
			default:
				d.state = state - 6
			}
			continue
		}

		var length uint32
		if d.bit(&d.isRep[state]) != 0 {
			if d.pos == 0 {
				return xzErr("LZMA repeat before any data")
			}
			if d.bit(&d.isRepG0[state]) == 0 {
				if d.bit(&d.isRep0Long[state][posState]) == 0 {
					// Short rep: one byte from the last distance.
					if err := d.copyMatch(d.rep[0], 1, end); err != nil {
						return err
					}
					d.state = 9
					if state >= 7 {
						d.state = 11
					}
					continue
				}
			} else {
				var dist uint32
				if d.bit(&d.isRepG1[state]) == 0 {
					dist = d.rep[1]
				} else {
					if d.bit(&d.isRepG2[state]) == 0 {
						dist = d.rep[2]
					} else {
						dist = d.rep[3]
						d.rep[3] = d.rep[2]
					}
					d.rep[2] = d.rep[1]
				}
				d.rep[1] = d.rep[0]
				d.rep[0] = dist
			}
			length = d.length(&d.repLenDec, posState)
			d.state = 8
			if state >= 7 {
				d.state = 11
			}
		} else {
			d.rep[3], d.rep[2], d.rep[1] = d.rep[2], d.rep[1], d.rep[0]
			length = d.length(&d.lenDec, posState)
			d.state = 7
			if state >= 7 {
				d.state = 10
			}
			slot := d.tree(d.posSlot[min(length, lzmaLenToPos-1)][:], 6)
			dist := slot
			if slot >= lzmaStartPosIdx {
				directBits := uint(slot>>1 - 1)
				dist = (2 | slot&1) << directBits
				if slot < lzmaEndPosIdx {
					dist += d.reverseTree(d.posSpecial[dist-slot:], directBits)
				} else {
					dist += d.direct(uint32(directBits-lzmaAlignBits)) << lzmaAlignBits
					dist += d.reverseTree(d.posAlign[:], lzmaAlignBits)
				}
			}
			d.rep[0] = dist
		}
		if err := d.copyMatch(d.rep[0], int(length)+lzmaMatchMinLen, end); err != nil {
			return err
		}
	}
	return nil
}

// copyMatch appends n bytes starting dist+1 bytes back. Source and
// destination may overlap, so the copy is byte by byte in that case.
func (d *lzma2Decoder) copyMatch(dist uint32, n, end int) error {
	back := uint64(dist) + 1
	if back > d.pos || back > uint64(d.dictSize) || back > uint64(len(d.win)) {
		return xzErr("LZMA match distance is out of range")
	}
	if len(d.win)+n > end {
		// LZMA2 does not let a match cross a chunk boundary.
		return xzErr("LZMA match crosses the chunk end")
	}
	src := len(d.win) - int(back)
	if int(back) >= n {
		d.win = append(d.win, d.win[src:src+n]...)
	} else {
		for i := 0; i < n; i++ {
			d.win = append(d.win, d.win[src+i])
		}
	}
	d.pos += uint64(n)
	return nil
}
