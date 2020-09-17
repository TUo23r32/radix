// Package resp3 implements the upgraded redis RESP3 protocol, a plaintext
// protocol which is also binary safe and backwards compatible with the original
// RESP2 protocol.
//
// Redis uses the RESP protocol to communicate with its clients, but there's
// nothing about the protocol which ties it to redis, it could be used for
// almost anything.
//
// See https://github.com/antirez/RESP3 for more details on the protocol.
//
// In general attribute messages are transarently discarded in this package. The
// user can read them manually prior to the message they are attached to if they
// are expected and desired.
package resp3

import (
	"bufio"
	"bytes"
	"encoding"
	"fmt"
	"io"
	"math"
	"math/big"
	"reflect"
	"sort"
	"strconv"
	"sync"

	"errors"

	"github.com/mediocregopher/radix/v3/internal/bytesutil"
	"github.com/mediocregopher/radix/v3/resp"
)

var delim = []byte{'\r', '\n'}

// Prefix enumerates the possible RESP3 types by enumerating the different
// prefix bytes a RESP3 message might start with.
type Prefix byte

// Enumeration of each of RESP3 prefices.
var (
	// Simple type prefices.
	BlobStringPrefix     Prefix = '$'
	SimpleStringPrefix   Prefix = '+'
	SimpleErrorPrefix    Prefix = '-'
	NumberPrefix         Prefix = ':'
	NullPrefix           Prefix = '_'
	DoublePrefix         Prefix = ','
	BooleanPrefix        Prefix = '#'
	BlobErrorPrefix      Prefix = '!'
	VerbatimStringPrefix Prefix = '='
	BigNumberPrefix      Prefix = '('

	// Aggregated type prefices.
	ArrayHeaderPrefix     Prefix = '*'
	MapPrefix             Prefix = '%'
	SetHeaderPrefix       Prefix = '~'
	AttributeHeaderPrefix Prefix = '|'
	PushHeaderPrefix      Prefix = '>'

	// Streamed type prefices.
	StreamedAggregatedTypeEndPrefix Prefix = '.'
	StreamedStringChunkPrefix       Prefix = ';'
)

func (p Prefix) String() string {
	pStr := string(p)
	switch pStr {
	case string(BlobStringPrefix):
		return "blob-string"
	case string(SimpleStringPrefix):
		return "simple-string"
	case string(SimpleErrorPrefix):
		return "simple-error"
	case string(NumberPrefix):
		return "number"
	case string(NullPrefix):
		return "null"
	case string(DoublePrefix):
		return "double"
	case string(BooleanPrefix):
		return "boolean"
	case string(BlobErrorPrefix):
		return "blob-error"
	case string(VerbatimStringPrefix):
		return "verbatim-string"
	case string(BigNumberPrefix):
		return "big-number"
	case string(ArrayHeaderPrefix):
		return "array"
	case string(MapPrefix):
		return "map"
	case string(SetHeaderPrefix):
		return "set"
	case string(AttributeHeaderPrefix):
		return "attribute"
	case string(PushHeaderPrefix):
		return "push"
	case string(StreamedAggregatedTypeEndPrefix):
		return "streamed-aggregated-type-end"
	case string(StreamedStringChunkPrefix):
		return "streamed-string-chunk"
	default:
		return pStr
	}
}

func (p Prefix) doesPrefix(b []byte) bool {
	if len(b) == 0 {
		panic("empty byte slice should not be passed here, please submit a bug report")
	}

	return Prefix(b[0]) == p
}

var (
	nullRESP2Suffix    = []byte("-1\r")
	null               = []byte("_\r\n")
	booleanTrue        = []byte("#t\r\n")
	booleanFalse       = []byte("#f\r\n")
	streamHeaderSize   = []byte("?")
	streamedHeaderTail = []byte("?\r\n")
	streamAggEnd       = []byte(".\r\n")
	emptyAggTail       = []byte("0\r\n")

	infMsg = []byte("inf")
	infStr = []byte("+inf")
)

var (
	emptyStructT             = reflect.TypeOf(struct{}{})
	byteSliceT               = reflect.TypeOf([]byte{})
	runeSliceT               = reflect.TypeOf([]rune{})
	readerT                  = reflect.TypeOf(new(io.Reader)).Elem()
	lenReaderT               = reflect.TypeOf(new(resp.LenReader)).Elem()
	encodingTextMarshalerT   = reflect.TypeOf(new(encoding.TextMarshaler)).Elem()
	encodingBinaryMarshalerT = reflect.TypeOf(new(encoding.BinaryMarshaler)).Elem()
	respMarshalerT           = reflect.TypeOf(new(resp.Marshaler)).Elem()
)

var (
	byteReaderPool = sync.Pool{
		New: func() interface{} {
			return bytes.NewReader(nil)
		},
	}
	bufioReaderPool = sync.Pool{
		New: func() interface{} {
			return bufio.NewReader(nil)
		},
	}
)

////////////////////////////////////////////////////////////////////////////////

// l may be negative to indicate that elements should be discarded until a
// streamed aggregated end type message is encountered.
func discardMulti(br *bufio.Reader, l int) error {
	for i := 0; i < l || l < 0; i++ {
		if more, err := (Any{}).maybeUnmarshalRESP(br, l < 0); err != nil {
			return err
		} else if !more {
			return nil
		}
	}
	return nil
}

func discardAttribute(br *bufio.Reader) error {
	var attrHead AttributeHeader
	b, err := br.Peek(1)
	if err != nil {
		return err
	} else if !AttributeHeaderPrefix.doesPrefix(b) {
		return nil
	} else if err := attrHead.UnmarshalRESP(br); err != nil {
		return nil
	}

	return discardMulti(br, attrHead.NumPairs*2)
}

type errUnexpectedPrefix struct {
	Prefix         Prefix
	ExpectedPrefix Prefix
}

func (e errUnexpectedPrefix) Error() string {
	return fmt.Sprintf("expected prefix %q, got %q", e.ExpectedPrefix, e.Prefix)
}

// peekAndAssertPrefix will peek at the next incoming redis message and assert
// that it is of the type identified by the given prefix.
//
// If the message is a RESP error (and that wasn't the intended prefix) then it
// will be unmarshaled into the appropriate RESP error type and returned.
//
// If the message is a not a RESP error(except the intended prefix) it will be
// discarded and errUnexpectedPrefix will be returned.
//
// peekAndAssertPrefix will discard any preceding attribute message when called
// with discardAttr set
func peekAndAssertPrefix(br *bufio.Reader, expectedPrefix Prefix, discardAttr bool) error {
	if discardAttr {
		if err := discardAttribute(br); err != nil {
			return err
		}
	}

	b, err := br.Peek(1)
	if err != nil {
		return err
	} else if expectedPrefix.doesPrefix(b) {
		return nil
	} else if SimpleErrorPrefix.doesPrefix(b) {
		var respErr SimpleError
		if err := respErr.UnmarshalRESP(br); err != nil {
			return err
		}
		return resp.ErrConnUsable{Err: respErr}
	} else if BlobErrorPrefix.doesPrefix(b) {
		var respErr BlobError
		if err := respErr.UnmarshalRESP(br); err != nil {
			return err
		}
		return resp.ErrConnUsable{Err: respErr}
	} else if err := (Any{}).UnmarshalRESP(br); err != nil {
		return err
	}
	return resp.ErrConnUsable{Err: errUnexpectedPrefix{
		Prefix:         Prefix(b[0]),
		ExpectedPrefix: expectedPrefix,
	}}
}

// like peekAndAssertPrefix, but will consume the prefix if it is the correct
// one as well.
func readAndAssertPrefix(br *bufio.Reader, prefix Prefix, discardAttr bool) error {
	if err := peekAndAssertPrefix(br, prefix, discardAttr); err != nil {
		return err
	}
	_, err := br.Discard(1)
	return err
}

////////////////////////////////////////////////////////////////////////////////

// BlobStringBytes represents the blob string type in the RESP protocol using a
// go byte slice. A B value of nil is an empty string.
//
// BlobStringBytes can also be used as the header message of a streamed string.
// When used in that way it will be followed by one or more BlobStringChunk
// messages, ending in a BlobStringChunk with a zero length.
type BlobStringBytes struct {
	B []byte

	// StreamedStringHeader indicates that this message is the header message of
	// a streamed string. It is mutually exclusive with B.
	StreamedStringHeader bool
}

// MarshalRESP implements the method for resp.Marshaler.
func (b BlobStringBytes) MarshalRESP(w io.Writer) error {
	scratch := bytesutil.GetBytes()
	defer bytesutil.PutBytes(scratch)

	*scratch = append(*scratch, byte(BlobStringPrefix))
	if b.StreamedStringHeader {
		*scratch = append(*scratch, streamHeaderSize...)
	} else {
		*scratch = strconv.AppendInt(*scratch, int64(len(b.B)), 10)
		*scratch = append(*scratch, delim...)
		*scratch = append(*scratch, b.B...)
	}
	*scratch = append(*scratch, delim...)
	_, err := w.Write(*scratch)
	return err
}

// UnmarshalRESP implements the method for resp.Unmarshaler.
func (b *BlobStringBytes) UnmarshalRESP(br *bufio.Reader) error {
	if err := readAndAssertPrefix(br, BlobStringPrefix, true); err != nil {
		return err
	}

	byt, err := bytesutil.ReadBytesDelim(br)
	if err != nil {
		return err
	} else if bytes.Equal(byt, streamHeaderSize) {
		b.B = nil
		b.StreamedStringHeader = true
		return nil
	}

	n, err := bytesutil.ParseInt(byt)
	if err != nil {
		return err
	} else if n == -1 {
		return errors.New("BlobStringBytes does not support unmarshaling RESP2 null bulk string")
	} else if n < 0 {
		return fmt.Errorf("invalid blob string length: %d", n)
	} else if n == 0 {
		b.B = []byte{}
	} else {
		b.B = bytesutil.Expand(b.B, int(n))
		if _, err := io.ReadFull(br, b.B); err != nil {
			return err
		}
	}

	if _, err := bytesutil.ReadBytesDelim(br); err != nil {
		return err
	}
	return nil
}

////////////////////////////////////////////////////////////////////////////////

// BlobString represents the blob string type in the RESP protocol using a go
// string.
//
// BlobString can also be used as the header message of a streamed string. When
// used in that way it will be followed by one or more BlobStringChunk messages,
// ending in a BlobStringChunk with a zero length.
type BlobString struct {
	S string

	// StreamedStringHeader indicates that this message is the header message of
	// a streamed string. It is mutually exclusive with S.
	StreamedStringHeader bool
}

// MarshalRESP implements the method for resp.Marshaler.
func (b BlobString) MarshalRESP(w io.Writer) error {
	scratch := bytesutil.GetBytes()
	defer bytesutil.PutBytes(scratch)

	*scratch = append(*scratch, byte(BlobStringPrefix))
	if b.StreamedStringHeader {
		*scratch = append(*scratch, streamHeaderSize...)
	} else {
		*scratch = strconv.AppendInt(*scratch, int64(len(b.S)), 10)
		*scratch = append(*scratch, delim...)
		*scratch = append(*scratch, b.S...)
	}
	*scratch = append(*scratch, delim...)
	_, err := w.Write(*scratch)
	return err
}

// UnmarshalRESP implements the method for resp.Unmarshaler.
func (b *BlobString) UnmarshalRESP(br *bufio.Reader) error {
	if err := readAndAssertPrefix(br, BlobStringPrefix, true); err != nil {
		return err
	}

	byt, err := bytesutil.ReadBytesDelim(br)
	if err != nil {
		return err
	} else if bytes.Equal(byt, streamHeaderSize) {
		b.S = ""
		b.StreamedStringHeader = true
		return nil
	}

	n, err := bytesutil.ParseInt(byt)
	if err != nil {
		return err
	} else if n == -1 {
		return errors.New("BlobString does not support unmarshaling RESP2 null bulk string")
	} else if n < 0 {
		return fmt.Errorf("invalid blob string length: %d", n)
	} else if n == 0 {
		b.S = ""
	} else {
		scratch := bytesutil.GetBytes()
		defer bytesutil.PutBytes(scratch)

		*scratch = bytesutil.Expand(*scratch, int(n))
		if _, err := io.ReadFull(br, *scratch); err != nil {
			return err
		}
		b.S = string(*scratch)
	}

	if _, err := bytesutil.ReadBytesDelim(br); err != nil {
		return err
	}

	return nil
}

////////////////////////////////////////////////////////////////////////////////

// BlobStringWriter represents a blob string in the RESP protocol.
//
// BlobStringWriter only supports marshalling and will use the given LenReader
// to do so.
type BlobStringWriter struct {
	LR resp.LenReader
}

// MarshalRESP implements the method for resp.Marshaler.
func (b BlobStringWriter) MarshalRESP(w io.Writer) error {
	scratch := bytesutil.GetBytes()
	defer bytesutil.PutBytes(scratch)

	l := int64(b.LR.Len())
	*scratch = append(*scratch, byte(BlobStringPrefix))
	*scratch = strconv.AppendInt(*scratch, l, 10)
	*scratch = append(*scratch, delim...)
	_, err := w.Write(*scratch)
	if err != nil {
		return err
	}

	if _, err := io.CopyN(w, b.LR, l); err != nil {
		return err
	} else if _, err := w.Write(delim); err != nil {
		return err
	}
	return nil
}

////////////////////////////////////////////////////////////////////////////////

// SimpleString represents the simple string type in the RESP protocol.
type SimpleString struct {
	S string
}

// MarshalRESP implements the method for resp.Marshaler.
func (ss SimpleString) MarshalRESP(w io.Writer) error {
	scratch := bytesutil.GetBytes()
	defer bytesutil.PutBytes(scratch)

	*scratch = append(*scratch, byte(SimpleStringPrefix))
	*scratch = append(*scratch, ss.S...)
	*scratch = append(*scratch, delim...)
	_, err := w.Write(*scratch)
	return err
}

// UnmarshalRESP implements the method for resp.Unmarshaler.
func (ss *SimpleString) UnmarshalRESP(br *bufio.Reader) error {
	if err := readAndAssertPrefix(br, SimpleStringPrefix, true); err != nil {
		return err
	}
	b, err := bytesutil.ReadBytesDelim(br)
	if err != nil {
		return err
	}

	ss.S = string(b)
	return nil
}

////////////////////////////////////////////////////////////////////////////////

// SimpleError represents the simple error type in the RESP protocol.
//
// SimpleError represents an actual error message being read/written on the
// wire, it is separate from network or parsing errors.
type SimpleError struct {
	S string
}

func (e SimpleError) Error() string {
	return e.S
}

// MarshalRESP implements the method for resp.Marshaler.
func (e SimpleError) MarshalRESP(w io.Writer) error {
	scratch := bytesutil.GetBytes()
	defer bytesutil.PutBytes(scratch)

	*scratch = append(*scratch, byte(SimpleErrorPrefix))
	*scratch = append(*scratch, e.S...)
	*scratch = append(*scratch, delim...)
	_, err := w.Write(*scratch)
	return err
}

// UnmarshalRESP implements the method for resp.Unmarshaler.
func (e *SimpleError) UnmarshalRESP(br *bufio.Reader) error {
	if err := readAndAssertPrefix(br, SimpleErrorPrefix, true); err != nil {
		return err
	}
	b, err := bytesutil.ReadBytesDelim(br)
	e.S = string(b)
	return err
}

////////////////////////////////////////////////////////////////////////////////

// Number represents the number type in the RESP protocol.
type Number struct {
	N int64
}

// MarshalRESP implements the method for resp.Marshaler.
func (n Number) MarshalRESP(w io.Writer) error {
	scratch := bytesutil.GetBytes()
	defer bytesutil.PutBytes(scratch)

	*scratch = append(*scratch, byte(NumberPrefix))
	*scratch = strconv.AppendInt(*scratch, n.N, 10)
	*scratch = append(*scratch, delim...)
	_, err := w.Write(*scratch)
	return err
}

// UnmarshalRESP implements the method for resp.Unmarshaler.
func (n *Number) UnmarshalRESP(br *bufio.Reader) error {
	if err := readAndAssertPrefix(br, NumberPrefix, true); err != nil {
		return err
	}
	i, err := bytesutil.ReadIntDelim(br)
	n.N = i
	return err
}

////////////////////////////////////////////////////////////////////////////////

// Null represents the null type in the RESP protocol.
//
// Null will always marshal to the RESP3 null type, but for convenience is also
// capable of unmarshaling the RESP2 null bulk string and null array values.
type Null struct{}

// MarshalRESP implements the method for resp.Marshaler.
func (Null) MarshalRESP(w io.Writer) error {
	_, err := w.Write(null)
	return err
}

// UnmarshalRESP implements the method for resp.Unmarshaler.
func (*Null) UnmarshalRESP(br *bufio.Reader) error {
	if err := discardAttribute(br); err != nil {
		return err
	}

	b, err := br.Peek(1)
	if err != nil {
		return err
	}
	prefix := Prefix(b[0])

	switch prefix {
	case NullPrefix:
		b, err := bytesutil.ReadBytesDelim(br)
		if err != nil {
			return err
		} else if len(b) != 1 {
			return errors.New("malformed null resp")
		}
		return nil

	case ArrayHeaderPrefix, BlobStringPrefix:
		// no matter what size an array or blob string is it _must_ have at
		// least 4 characters on the wire (prefix+size+delim). So only check
		// that.
		b, err := br.Peek(4)
		if err != nil {
			return err
		} else if !bytes.Equal(b[1:], nullRESP2Suffix) {
			if err := (Any{}).UnmarshalRESP(br); err != nil {
				return err
			}
			return resp.ErrConnUsable{Err: errors.New("malformed null resp")}
		}

		// actually consume the message, after all this peeking.
		_, err = bytesutil.ReadBytesDelim(br)
		return err

	default:
		if err := (Any{}).UnmarshalRESP(br); err != nil {
			return err
		}
		return resp.ErrConnUsable{Err: errUnexpectedPrefix{
			Prefix:         prefix,
			ExpectedPrefix: NullPrefix,
		}}
	}
}

////////////////////////////////////////////////////////////////////////////////

// Double represents the double type in the RESP protocol.
type Double struct {
	F float64
}

// MarshalRESP implements the method for resp.Marshaler.
func (d Double) MarshalRESP(w io.Writer) error {
	scratch := bytesutil.GetBytes()
	*scratch = append(*scratch, byte(DoublePrefix))

	if math.IsInf(d.F, 1) {
		*scratch = append(*scratch, "inf"...)
	} else if math.IsInf(d.F, -1) {
		*scratch = append(*scratch, "-inf"...)
	} else {
		*scratch = strconv.AppendFloat(*scratch, d.F, 'f', -1, 64)
	}

	*scratch = append(*scratch, delim...)
	_, err := w.Write(*scratch)
	bytesutil.PutBytes(scratch)
	return err
}

// UnmarshalRESP implements the method for resp.Unmarshaler.
func (d *Double) UnmarshalRESP(br *bufio.Reader) error {
	if err := readAndAssertPrefix(br, DoublePrefix, true); err != nil {
		return err
	}
	b, err := bytesutil.ReadBytesDelim(br)
	if err != nil {
		return err
	} else if d.F, err = strconv.ParseFloat(string(b), 64); err != nil {
		return resp.ErrConnUsable{
			Err: fmt.Errorf("failed to parse double resp %q as float: %w", b, err),
		}
	}
	return nil
}

////////////////////////////////////////////////////////////////////////////////

// Boolean represents the boolean type in the RESP protocol.
type Boolean struct {
	B bool
}

// MarshalRESP implements the method for resp.Marshaler.
func (b Boolean) MarshalRESP(w io.Writer) error {
	var err error
	if b.B {
		_, err = w.Write(booleanTrue)
	} else {
		_, err = w.Write(booleanFalse)
	}
	return err
}

// UnmarshalRESP implements the method for resp.Unmarshaler.
func (b *Boolean) UnmarshalRESP(br *bufio.Reader) error {
	if err := readAndAssertPrefix(br, BooleanPrefix, true); err != nil {
		return err
	}
	byt, err := bytesutil.ReadBytesDelim(br)
	if err != nil {
		return err
	} else if len(byt) != 1 {
		return errors.New("malformed boolean resp")
	} else if byt[0] == 't' {
		b.B = true
	} else if byt[0] == 'f' {
		b.B = false
	} else {
		return errors.New("malformed boolean resp")
	}
	return nil
}

////////////////////////////////////////////////////////////////////////////////

// BlobError represents the blob error type in the RESP protocol.
//
// BlobError only represents an actual error message being read/written on the
// wire, it is separate from network or parsing errors.
type BlobError struct {
	B []byte
}

func (e BlobError) Error() string {
	return string(e.B)
}

// MarshalRESP implements the method for resp.Marshaler.
func (e BlobError) MarshalRESP(w io.Writer) error {
	scratch := bytesutil.GetBytes()
	defer bytesutil.PutBytes(scratch)

	*scratch = append(*scratch, byte(BlobErrorPrefix))
	*scratch = strconv.AppendInt(*scratch, int64(len(e.B)), 10)
	*scratch = append(*scratch, delim...)
	*scratch = append(*scratch, e.B...)
	*scratch = append(*scratch, delim...)
	_, err := w.Write(*scratch)
	return err
}

// UnmarshalRESP implements the method for resp.Unmarshaler.
func (e *BlobError) UnmarshalRESP(br *bufio.Reader) error {
	if err := readAndAssertPrefix(br, BlobErrorPrefix, true); err != nil {
		return err
	}

	n, err := bytesutil.ReadIntDelim(br)
	if err != nil {
		return err
	} else if n < 0 {
		return fmt.Errorf("invalid blob error length: %d", n)
	}

	e.B = bytesutil.Expand(e.B, int(n))
	if _, err := io.ReadFull(br, e.B); err != nil {
		return err
	} else if _, err := bytesutil.ReadBytesDelim(br); err != nil {
		return err
	}

	return nil
}

////////////////////////////////////////////////////////////////////////////////

// VerbatimStringBytes represents the verbatim string type in the RESP protocol
// using a go byte slice. A B value of nil is an empty string.
type VerbatimStringBytes struct {
	B []byte

	// Format is a 3 character string describing the format that the verbatim
	// string is encoded in, e.g. "txt" or "mkd". If Format is not exactly 3
	// characters then MarshalRESP will error without writing anything.
	Format []byte
}

// MarshalRESP implements the method for resp.Marshaler.
func (b VerbatimStringBytes) MarshalRESP(w io.Writer) error {
	if len(b.Format) != 3 {
		return resp.ErrConnUsable{
			Err: errors.New("Format field must be exactly 3 characters"),
		}
	}
	scratch := bytesutil.GetBytes()
	defer bytesutil.PutBytes(scratch)

	*scratch = append(*scratch, byte(VerbatimStringPrefix))
	*scratch = strconv.AppendInt(*scratch, int64(len(b.B))+4, 10)
	*scratch = append(*scratch, delim...)
	*scratch = append(*scratch, b.Format...)
	*scratch = append(*scratch, ':')
	*scratch = append(*scratch, b.B...)
	*scratch = append(*scratch, delim...)
	_, err := w.Write(*scratch)
	return err
}

// UnmarshalRESP implements the method for resp.Unmarshaler.
func (b *VerbatimStringBytes) UnmarshalRESP(br *bufio.Reader) error {
	if err := readAndAssertPrefix(br, VerbatimStringPrefix, true); err != nil {
		return err
	}

	n, err := bytesutil.ReadIntDelim(br)
	if err != nil {
		return err
	} else if n < 4 {
		return fmt.Errorf("invalid verbatim string length: %d", n)
	}

	b.B = bytesutil.Expand(b.B, int(n))
	if _, err := io.ReadFull(br, b.B); err != nil {
		return err
	} else if _, err := bytesutil.ReadBytesDelim(br); err != nil {
		return err
	}

	b.Format, b.B = b.B[:3], b.B[4:]
	return nil
}

////////////////////////////////////////////////////////////////////////////////

// VerbatimString represents the verbatim string type in the RESP protocol
// using a go string.
type VerbatimString struct {
	S string

	// Format is a 3 character string describing the format that the verbatim
	// string is encoded in, e.g. "txt" or "mkd". If Format is not exactly 3
	// characters then MarshalRESP will error without writing anything.
	Format string
}

// MarshalRESP implements the method for resp.Marshaler.
func (b VerbatimString) MarshalRESP(w io.Writer) error {
	if len(b.Format) != 3 {
		return resp.ErrConnUsable{
			Err: errors.New("Format field must be exactly 3 characters"),
		}
	}
	scratch := bytesutil.GetBytes()
	defer bytesutil.PutBytes(scratch)

	*scratch = append(*scratch, byte(VerbatimStringPrefix))
	*scratch = strconv.AppendInt(*scratch, int64(len(b.S))+4, 10)
	*scratch = append(*scratch, delim...)
	*scratch = append(*scratch, b.Format...)
	*scratch = append(*scratch, ':')
	*scratch = append(*scratch, b.S...)
	*scratch = append(*scratch, delim...)
	_, err := w.Write(*scratch)
	return err
}

// UnmarshalRESP implements the method for resp.Unmarshaler.
func (b *VerbatimString) UnmarshalRESP(br *bufio.Reader) error {
	if err := readAndAssertPrefix(br, VerbatimStringPrefix, true); err != nil {
		return err
	}

	n, err := bytesutil.ReadIntDelim(br)
	if err != nil {
		return err
	} else if n < 4 {
		return fmt.Errorf("invalid verbatim string length: %d", n)
	}

	scratch := bytesutil.GetBytes()
	defer bytesutil.PutBytes(scratch)

	*scratch = bytesutil.Expand(*scratch, int(n))
	if _, err := io.ReadFull(br, *scratch); err != nil {
		return err
	} else if _, err := bytesutil.ReadBytesDelim(br); err != nil {
		return err
	}

	b.Format = string((*scratch)[:3])
	b.S = string((*scratch)[4:])
	return nil
}

////////////////////////////////////////////////////////////////////////////////

// BigNumber represents the big number type in the RESP protocol. Marshaling a
// nil I value will cause a panic.
type BigNumber struct {
	I *big.Int
}

// MarshalRESP implements the method for resp.Marshaler.
func (b BigNumber) MarshalRESP(w io.Writer) error {
	scratch := bytesutil.GetBytes()
	defer bytesutil.PutBytes(scratch)

	*scratch = append(*scratch, byte(BigNumberPrefix))
	*scratch = b.I.Append(*scratch, 10)
	*scratch = append(*scratch, delim...)
	_, err := w.Write(*scratch)
	return err
}

// UnmarshalRESP implements the method for resp.Unmarshaler.
func (b *BigNumber) UnmarshalRESP(br *bufio.Reader) error {
	if err := readAndAssertPrefix(br, BigNumberPrefix, true); err != nil {
		return err
	}

	byt, err := bytesutil.ReadBytesDelim(br)
	if err != nil {
		return err
	} else if b.I == nil {
		b.I = new(big.Int)
	}

	var ok bool
	if b.I, ok = b.I.SetString(string(byt), 10); !ok {
		return fmt.Errorf("malformed big number: %q", byt)
	}
	return nil
}

////////////////////////////////////////////////////////////////////////////////

func marshalAggHeader(w io.Writer, prefix Prefix, n int, streamHeader bool) error {
	scratch := bytesutil.GetBytes()
	defer bytesutil.PutBytes(scratch)

	*scratch = append(*scratch, byte(prefix))
	if streamHeader {
		*scratch = append(*scratch, streamHeaderSize...)
	} else {
		*scratch = strconv.AppendInt(*scratch, int64(n), 10)
	}
	*scratch = append(*scratch, delim...)
	_, err := w.Write(*scratch)
	return err
}

func unmarshalAggHeader(br *bufio.Reader, prefix Prefix, n *int, streamHeader *bool, discardAttr bool) error {
	if err := readAndAssertPrefix(br, prefix, discardAttr); err != nil {
		return err
	}

	b, err := bytesutil.ReadBytesDelim(br)
	if err != nil {
		return err
	} else if streamHeader != nil {
		if *streamHeader = bytes.Equal(b, streamHeaderSize); *streamHeader {
			*n = 0
			return nil
		}
		*streamHeader = false
	}

	n64, err := bytesutil.ParseInt(b)
	if err != nil {
		return err
	} else if n64 < 0 {
		return fmt.Errorf("invalid number of elements: %d", n64)
	}

	*n = int(n64)
	return nil
}

////////////////////////////////////////////////////////////////////////////////

// ArrayHeader represents the header sent preceding array elements in the RESP
// protocol. ArrayHeader only declares how many elements are in the array
// message.
//
// ArrayHeader can also be used as the header of a streamed array, whose size is
// not known in advance, by setting StreamedArrayHeader instead of NumElems.
type ArrayHeader struct {
	NumElems int

	// StreamedArrayHeader indicates that this message is the header message of
	// a streamed array. It is mutually exclusive with NumElems.
	StreamedArrayHeader bool
}

// MarshalRESP implements the method for resp.Marshaler.
func (h ArrayHeader) MarshalRESP(w io.Writer) error {
	return marshalAggHeader(w, ArrayHeaderPrefix, h.NumElems, h.StreamedArrayHeader)
}

// UnmarshalRESP implements the method for resp.Unmarshaler.
func (h *ArrayHeader) UnmarshalRESP(br *bufio.Reader) error {
	return unmarshalAggHeader(br, ArrayHeaderPrefix, &h.NumElems, &h.StreamedArrayHeader, true)
}

////////////////////////////////////////////////////////////////////////////////

// MapHeader represents the header sent preceding map elements in the RESP
// protocol. MapHeader only declares how many elements are in the map message.
//
// MapHeader can also be used as the header of a streamed array, whose size is
// not known in advance, by setting StreamedMapHeader instead of NumElems.
type MapHeader struct {
	NumPairs int

	// StreamedMapHeader indicates that this message is the header message of
	// a streamed map. It is mutually exclusive with NumPairs.
	StreamedMapHeader bool
}

// MarshalRESP implements the method for resp.Marshaler.
func (h MapHeader) MarshalRESP(w io.Writer) error {
	return marshalAggHeader(w, MapPrefix, h.NumPairs, h.StreamedMapHeader)
}

// UnmarshalRESP implements the method for resp.Unmarshaler.
func (h *MapHeader) UnmarshalRESP(br *bufio.Reader) error {
	return unmarshalAggHeader(br, MapPrefix, &h.NumPairs, &h.StreamedMapHeader, true)
}

////////////////////////////////////////////////////////////////////////////////

// SetHeader represents the header sent preceding set elements in the RESP
// protocol. SetHeader only declares how many elements are in the set message.
//
// SetHeader can also be used as the header of a streamed array, whose size is
// not known in advance, by setting StreamedSetHeader instead of NumElems.
type SetHeader struct {
	NumElems int

	// StreamedSetHeader indicates that this message is the header message of
	// a streamed set. It is mutually exclusive with NumElems.
	StreamedSetHeader bool
}

// MarshalRESP implements the method for resp.Marshaler.
func (h SetHeader) MarshalRESP(w io.Writer) error {
	return marshalAggHeader(w, SetHeaderPrefix, h.NumElems, h.StreamedSetHeader)
}

// UnmarshalRESP implements the method for resp.Unmarshaler.
func (h *SetHeader) UnmarshalRESP(br *bufio.Reader) error {
	return unmarshalAggHeader(br, SetHeaderPrefix, &h.NumElems, &h.StreamedSetHeader, true)
}

////////////////////////////////////////////////////////////////////////////////

// AttributeHeader represents the header sent preceding attribute elements in
// the RESP protocol. AttributeHeader only declares how many elements are in the
// attribute message.
type AttributeHeader struct {
	NumPairs int
}

// MarshalRESP implements the method for resp.Marshaler.
func (h AttributeHeader) MarshalRESP(w io.Writer) error {
	return marshalAggHeader(w, AttributeHeaderPrefix, h.NumPairs, false)
}

// UnmarshalRESP implements the method for resp.Unmarshaler.
func (h *AttributeHeader) UnmarshalRESP(br *bufio.Reader) error {
	return unmarshalAggHeader(br, AttributeHeaderPrefix, &h.NumPairs, nil, false)
}

////////////////////////////////////////////////////////////////////////////////

// PushHeader represents the header sent preceding push elements in the RESP
// protocol. PushHeader only declares how many elements are in the push message.
type PushHeader struct {
	NumElems int
}

// MarshalRESP implements the method for resp.Marshaler.
func (h PushHeader) MarshalRESP(w io.Writer) error {
	return marshalAggHeader(w, PushHeaderPrefix, h.NumElems, false)
}

// UnmarshalRESP implements the method for resp.Unmarshaler.
func (h *PushHeader) UnmarshalRESP(br *bufio.Reader) error {
	return unmarshalAggHeader(br, PushHeaderPrefix, &h.NumElems, nil, true)
}

////////////////////////////////////////////////////////////////////////////////

// StreamedStringChunkBytes represents a streamed string chunk message in the
// RESP protocol using a byte slice. A slice with length zero indicates the end
// of the streamed string.
type StreamedStringChunkBytes struct {
	B []byte
}

// MarshalRESP implements the method for resp.Marshaler.
func (b StreamedStringChunkBytes) MarshalRESP(w io.Writer) error {
	scratch := bytesutil.GetBytes()
	defer bytesutil.PutBytes(scratch)

	l := int64(len(b.B))
	*scratch = append(*scratch, byte(StreamedStringChunkPrefix))
	*scratch = strconv.AppendInt(*scratch, l, 10)
	*scratch = append(*scratch, delim...)
	if l > 0 {
		*scratch = append(*scratch, b.B...)
		*scratch = append(*scratch, delim...)
	}
	_, err := w.Write(*scratch)
	return err
}

// UnmarshalRESP implements the method for resp.Unmarshaler.
func (b *StreamedStringChunkBytes) UnmarshalRESP(br *bufio.Reader) error {
	if err := readAndAssertPrefix(br, StreamedStringChunkPrefix, true); err != nil {
		return err
	}

	n, err := bytesutil.ReadIntDelim(br)
	if err != nil {
		return err
	} else if n < 0 {
		return fmt.Errorf("invalid streamed string chunk length: %d", n)
	} else if n == 0 {
		b.B = []byte{}
	} else {
		b.B = bytesutil.Expand(b.B, int(n))
		if _, err := io.ReadFull(br, b.B); err != nil {
			return err
		} else if _, err := bytesutil.ReadBytesDelim(br); err != nil {
			return err
		}
	}

	return nil
}

////////////////////////////////////////////////////////////////////////////////

// StreamedStringChunk represents a streamed string chunk message in the RESP
// protocol using a go string. An empty string indicates the end of the streamed
// string.
type StreamedStringChunk struct {
	S string
}

// MarshalRESP implements the method for resp.Marshaler.
func (b StreamedStringChunk) MarshalRESP(w io.Writer) error {
	scratch := bytesutil.GetBytes()
	defer bytesutil.PutBytes(scratch)

	l := int64(len(b.S))
	*scratch = append(*scratch, byte(StreamedStringChunkPrefix))
	*scratch = strconv.AppendInt(*scratch, l, 10)
	*scratch = append(*scratch, delim...)
	if l > 0 {
		*scratch = append(*scratch, b.S...)
		*scratch = append(*scratch, delim...)
	}
	_, err := w.Write(*scratch)
	return err
}

// UnmarshalRESP implements the method for resp.Unmarshaler.
func (b *StreamedStringChunk) UnmarshalRESP(br *bufio.Reader) error {
	if err := readAndAssertPrefix(br, StreamedStringChunkPrefix, true); err != nil {
		return err
	}

	n, err := bytesutil.ReadIntDelim(br)
	if err != nil {
		return err
	} else if n < 0 {
		return fmt.Errorf("invalid streamed string chunk length: %d", n)
	} else if n == 0 {
		b.S = ""
	} else {
		scratch := bytesutil.GetBytes()
		defer bytesutil.PutBytes(scratch)

		*scratch = bytesutil.Expand(*scratch, int(n))
		if _, err := io.ReadFull(br, *scratch); err != nil {
			return err
		} else if _, err := bytesutil.ReadBytesDelim(br); err != nil {
			return err
		}
		b.S = string(*scratch)
	}

	return nil
}

////////////////////////////////////////////////////////////////////////////////

// streamedStringScratchSize indicates how large of a scratch buffer should be
// used for calling Read/Write methods in the StreamedStringReader/Writer types.
// It's useful for this to be declared as a constant to help make tests
// deterministic.
const streamedStringScratchSize = 1024

// StreamedStringReader implements reading a streamed string RESP message off
// the wire and writing the string being streamed onto the given io.Writer.
//
// UnmarshalRESP will block until the entire streamed string has been copied
// onto the given io.Writer.
type StreamedStringReader struct {
	W io.Writer

	// Buffer will be used for reading streamed string chunks into. If nil then
	// a buffer of reasonable size will retrieved from a pool and be used
	// instead.
	Buffer []byte
}

// UnmarshalRESP implements the method for resp.Unmarshaler.
func (r *StreamedStringReader) UnmarshalRESP(br *bufio.Reader) error {
	if err := peekAndAssertPrefix(br, StreamedStringChunkPrefix, true); errors.As(err, new(errUnexpectedPrefix)) {
		// the first message in a stream will be a blob string with a size of
		// "?". Discard that message if it comes up.
		//
		// Technically this would also discard a "?" sized blob string which
		// showed up in the middle of a stream, as well as allow a streamed
		// string with no prefix at all. Not ideal, but it would require some
		// state on the StreamedStringReader to fix it, which we don't do for
		// any other Unmarshalers.
		if err := peekAndAssertPrefix(br, BlobStringPrefix, true); err != nil {
			return err
		}
		var u BlobStringBytes
		if err := u.UnmarshalRESP(br); err != nil {
			return err
		} else if !u.StreamedStringHeader {
			return errors.New("sized blob string received instead of an unknown sized streamed string")
		}
	}

	scratch := &r.Buffer
	if len(*scratch) == 0 {
		scratch = bytesutil.GetBytes()
		defer bytesutil.PutBytes(scratch)
		*scratch = bytesutil.Expand(*scratch, streamedStringScratchSize)
	}

	chunkBytes := StreamedStringChunkBytes{B: *scratch}
	for {
		if err := chunkBytes.UnmarshalRESP(br); err != nil {
			return err
		} else if len(chunkBytes.B) == 0 {
			return nil
		} else if _, err := r.W.Write(chunkBytes.B); err != nil {
			return err
		}
	}
}

// StreamedStringWriter implements reading off of a given io.Reader
// and writing that data as a RESP streamed string message.
//
// MarshalRESP will block until the given io.Reader has returned io.EOF or some
// other error.
type StreamedStringWriter struct {
	R io.Reader

	// Buffer will be used for io.Read calls on the given io.Reader. If nil then
	// a buffer of reasonable size will retrieved from a pool and be used
	// instead.
	Buffer []byte
}

// MarshalRESP implements the method for resp.Marshaler.
func (sw StreamedStringWriter) MarshalRESP(w io.Writer) error {
	if err := (BlobStringBytes{StreamedStringHeader: true}).MarshalRESP(w); err != nil {
		return err
	}

	scratch := &sw.Buffer
	if len(*scratch) == 0 {
		scratch = bytesutil.GetBytes()
		defer bytesutil.PutBytes(scratch)
		*scratch = bytesutil.Expand(*scratch, streamedStringScratchSize)
	}

	for {
		if n, err := sw.R.Read(*scratch); errors.Is(err, io.EOF) {
			// marshal an empty chunk to indicate the end of the streamed string
			return (StreamedStringChunkBytes{}).MarshalRESP(w)
		} else if err != nil {
			return err
		} else if n == 0 {
			continue
		} else if (StreamedStringChunkBytes{B: (*scratch)[:n]}).MarshalRESP(w); err != nil {
			return err
		}
	}
}

////////////////////////////////////////////////////////////////////////////////

// StreamedAggregatedElement is a helper type used to unmarshal the elements of
// a streamed aggregated type (e.g. a streamed array) such that it is possible
// to check if the end of the stream has been reached.
type StreamedAggregatedElement struct {
	// Unmarshaler is unmarshaled into when the message being read isn't the
	// streamed aggregated end type.
	resp.Unmarshaler

	// End is set to true when the message read isn't the streamed aggregated
	// type end message. If End is true then the Unmarshaler was not touched.
	End bool
}

// UnmarshalRESP implements the method for resp.Unmarshaler.
func (s *StreamedAggregatedElement) UnmarshalRESP(br *bufio.Reader) error {
	b, err := br.Peek(len(streamAggEnd))
	if err != nil {
		return err
	} else if s.End = bytes.Equal(b, streamAggEnd); s.End {
		_, err = br.Discard(len(b))
		return err
	}
	return s.Unmarshaler.UnmarshalRESP(br)
}

// StreamedAggregatedTypeEnd represents a streamed aggregated end type message
// in the RESP protocol.
type StreamedAggregatedTypeEnd struct{}

// MarshalRESP implements the method for resp.Marshaler.
func (s StreamedAggregatedTypeEnd) MarshalRESP(w io.Writer) error {
	_, err := w.Write(streamAggEnd)
	return err
}

// UnmarshalRESP implements the method for resp.Unmarshaler.
func (s *StreamedAggregatedTypeEnd) UnmarshalRESP(br *bufio.Reader) error {
	if err := readAndAssertPrefix(br, StreamedAggregatedTypeEndPrefix, true); err != nil {
		return err
	}
	_, err := br.Discard(len(delim))
	return err
}

////////////////////////////////////////////////////////////////////////////////

// left may be negative to indicate that elements should be discarded until a
// streamed aggregated end type message is encountered.
func discardMultiAfterErr(br *bufio.Reader, left int, lastErr error) error {
	// if the last error which occurred didn't discard the message it was on, we
	// can't do anything
	if !errors.As(lastErr, new(resp.ErrConnUsable)) {
		return lastErr
	} else if err := discardMulti(br, left); err != nil {
		return err
	}

	// The original error was already wrapped in an ErrConnUsable, so just
	// return it as it was given
	return lastErr
}

// Any represents any primitive go type, such as integers, floats, strings,
// bools, etc... as well as some interface types such as io.Readers/Writers and
// encoding.Binary(Un)Marshalers. The doc strings on MarshalRESP and
// UnmarshalRESP list the exact mappings between RESP and go types.
type Any struct {
	I interface{}

	// MarshalDeterministic causes MarshalRESP to marshal maps and sets
	// deterministically.
	MarshalDeterministic bool
}

func (a Any) cp(i interface{}) Any {
	a.I = i
	return a
}

func isSetMap(t reflect.Type) bool {
	return t.Elem() == emptyStructT
}

// MarshalRESP implements the method for resp.Marshaler. The mappings from go
// types to RESP types are as follows (T denotes a go type, RT denotes the
// corresponding RESP type for T):
//
//	[]byte, string, []rune, resp.LenReader -> blob string
//	encoding.TextMarshaler                 -> blob string
//	encoding.BinaryMarshaler               -> blob string
//	io.Reader                              -> streamed string
//
//	nil, []T(nil), map[T]struct{}(nil), map[T]T'(nil) -> null
//	error                                             -> blob error
//
//	bool                                    -> boolean
//	float32, float64), big.Float            -> double
//	int, int8, int16, int32, int64, big.Int -> number
//	uint, uint8, uint16, uint32, uint64     -> number
//
//	*T             -> RT
//	[]T            -> array of RT
//	map[T]struct{} -> set of RT
//	map[T]T'       -> map with RT keys and RT' values
//
// Structs will be marshaled as a map, where each of the struct's field names
// will be marshaled as a simple string, and each of the struct's values will be
// marshaled as the RESP type corresponding to that value's type. Each field can
// be tagged with `redis:"fieldName"` to specify the field name manually, or
// `redis:"-"` to omit the field.
//
func (a Any) MarshalRESP(w io.Writer) error {
	if m, ok := a.I.(resp.Marshaler); ok {
		return m.MarshalRESP(w)
	}

	marshalBlobStr := func(b []byte) error {
		return BlobStringBytes{B: b}.MarshalRESP(w)
	}

	switch at := a.I.(type) {
	case []byte:
		return marshalBlobStr(at)
	case string:
		scratch := bytesutil.GetBytes()
		defer bytesutil.PutBytes(scratch)
		*scratch = append(*scratch, at...)
		return marshalBlobStr(*scratch)
	case []rune:
		scratch := bytesutil.GetBytes()
		defer bytesutil.PutBytes(scratch)
		*scratch = append(*scratch, string(at)...)
		return marshalBlobStr(*scratch)
	case bool:
		return Boolean{B: at}.MarshalRESP(w)
	case float32:
		return Double{F: float64(at)}.MarshalRESP(w)
	case float64:
		return Double{F: at}.MarshalRESP(w)
	case *big.Float:
		// big.Float is a TextMarshaler, so we have to catch it here so at
		// doesn't make it to that case.
		return a.cp(*at).MarshalRESP(w)
	case big.Float:
		f, accuracy := at.Float64()
		if accuracy != big.Exact {
			return resp.ErrConnUsable{
				Err: fmt.Errorf("could not marshal big.Float value %s into double", at.String()),
			}
		}
		return Double{F: f}.MarshalRESP(w)
	case nil:
		return Null{}.MarshalRESP(w)
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		at64 := bytesutil.AnyIntToInt64(at)
		return Number{N: at64}.MarshalRESP(w)
	case big.Int:
		// big.Int is a TextMarshaler, so we have to catch it here so at doesn't
		// make it to that case.
		return a.cp(&at).MarshalRESP(w)
	case *big.Int:
		return BigNumber{I: at}.MarshalRESP(w)
	case error:
		return BlobError{B: []byte(at.Error())}.MarshalRESP(w)
	case resp.LenReader:
		return BlobStringWriter{LR: at}.MarshalRESP(w)
	case io.Reader:
		return StreamedStringWriter{R: at}.MarshalRESP(w)
	case encoding.TextMarshaler:
		b, err := at.MarshalText()
		if err != nil {
			return err
		}
		return marshalBlobStr(b)
	case encoding.BinaryMarshaler:
		b, err := at.MarshalBinary()
		if err != nil {
			return err
		}
		return marshalBlobStr(b)
	}

	// now we use.... reflection! duhduhduuuuh....
	vv := reflect.ValueOf(a.I)

	// if it's a pointer we try to dereference down to a non-pointer element. If
	// we hit nil then we will generally marshal the null message, unless it's a
	// collection type (slice/array/map/struct) and MarshalNoAggHeaders is set
	// in which case we don't marshal anything.
	if vv.Kind() != reflect.Ptr {
		// ok
	} else if vv.IsNil() {
		return a.cp(nil).MarshalRESP(w)
	} else {
		return a.cp(vv.Elem().Interface()).MarshalRESP(w)
	}

	// some helper functions
	var err error
	var anyWritten bool
	setAnyWritten := func() {
		var errConnUsable resp.ErrConnUsable
		if !errors.As(err, &errConnUsable) {
			anyWritten = true
		}
	}
	arrHeader := func(l int) {
		if err != nil {
			return
		}
		err = ArrayHeader{NumElems: l}.MarshalRESP(w)
		setAnyWritten()
	}
	setHeader := func(l int) {
		if err != nil {
			return
		}
		err = SetHeader{NumElems: l}.MarshalRESP(w)
		setAnyWritten()
	}
	mapHeader := func(l int) {
		if err != nil {
			return
		}
		err = MapHeader{NumPairs: l}.MarshalRESP(w)
		setAnyWritten()
	}
	aggVal := func(v interface{}) {
		if err != nil {
			return
		}
		err = a.cp(v).MarshalRESP(w)
		setAnyWritten()
	}
	unwrapIfAnyWritten := func() {
		if anyWritten {
			err = resp.ErrConnUnusable(err)
		}
	}

	switch vv.Kind() {
	case reflect.Slice, reflect.Array:
		if vv.IsNil() {
			return a.cp(nil).MarshalRESP(w)
		}
		l := vv.Len()
		arrHeader(l)
		for i := 0; i < l; i++ {
			aggVal(vv.Index(i).Interface())
		}
		unwrapIfAnyWritten()

	case reflect.Map:
		if vv.IsNil() {
			return a.cp(nil).MarshalRESP(w)
		}
		kkv := vv.MapKeys()
		if a.MarshalDeterministic {
			// This is hacky af but basically works
			sort.Slice(kkv, func(i, j int) bool {
				return fmt.Sprint(kkv[i].Interface()) < fmt.Sprint(kkv[j].Interface())
			})
		}

		setMap := isSetMap(vv.Type())
		if setMap {
			setHeader(len(kkv))
		} else {
			mapHeader(len(kkv))
		}

		for _, kv := range kkv {
			aggVal(kv.Interface())
			if !setMap {
				aggVal(vv.MapIndex(kv).Interface())
			}
		}
		unwrapIfAnyWritten()

	case reflect.Struct:
		return a.marshalStruct(w, vv, false)

	default:
		return resp.ErrConnUsable{
			Err: fmt.Errorf("could not marshal value of type %T", a.I),
		}
	}

	return err
}

// numStructFields returns the number of fields in a struct.
func numStructFields(vv reflect.Value) (int, error) {
	tt := vv.Type()
	l := vv.NumField()
	var fields int
	for i := 0; i < l; i++ {
		ft, fv := tt.Field(i), vv.Field(i)
		if ft.Anonymous {
			if fv = reflect.Indirect(fv); fv.IsValid() { // fv isn't nil
				innerFields, err := numStructFields(fv)
				if err != nil {
					return 0, err
				}
				fields += innerFields
			}
			continue
		} else if ft.PkgPath != "" || ft.Tag.Get("redis") == "-" {
			continue // continue
		}

		fields++
	}
	return fields, nil
}

func (a Any) marshalStruct(w io.Writer, vv reflect.Value, inline bool) error {
	if !inline {
		numFields, err := numStructFields(vv)
		if err != nil {
			return err
		} else if err = (MapHeader{NumPairs: numFields}).MarshalRESP(w); err != nil {
			return err
		}
	}

	tt := vv.Type()
	l := vv.NumField()
	for i := 0; i < l; i++ {
		ft, fv := tt.Field(i), vv.Field(i)
		tag := ft.Tag.Get("redis")
		if ft.Anonymous {
			if fv = reflect.Indirect(fv); !fv.IsValid() { // fv is nil
				continue
			} else if err := a.marshalStruct(w, fv, true); err != nil {
				return err
			}
			continue
		} else if ft.PkgPath != "" || tag == "-" {
			continue // unexported
		}

		keyName := ft.Name
		if tag != "" {
			keyName = tag
		}
		if err := (SimpleString{S: keyName}).MarshalRESP(w); err != nil {
			return err
		} else if err := a.cp(fv.Interface()).MarshalRESP(w); err != nil {
			return err
		}
	}
	return nil
}

func saneDefault(prefix Prefix) (interface{}, error) {
	// we don't handle ErrorPrefix because that always returns an error and
	// doesn't touch I
	switch prefix {
	case BlobStringPrefix:
		bb := make([]byte, 16)
		return &bb, nil
	case SimpleStringPrefix:
		return new(string), nil
	case NumberPrefix:
		return new(int64), nil
	case DoublePrefix:
		return new(float64), nil
	case BooleanPrefix:
		return new(bool), nil
	case VerbatimStringPrefix:
		bb := make([]byte, 16)
		return &bb, nil
	case BigNumberPrefix:
		return new(*big.Int), nil
	case ArrayHeaderPrefix, PushHeaderPrefix:
		ii := make([]interface{}, 8)
		return &ii, nil
	case MapPrefix:
		return &map[interface{}]interface{}{}, nil
	case SetHeaderPrefix:
		return &map[interface{}]struct{}{}, nil
	case AttributeHeaderPrefix:
		return &map[string]interface{}{}, nil
	default:
		return nil, resp.ErrConnUsable{Err: fmt.Errorf("unexpected prefix: %q", prefix)}
	}
}

// UnmarshalRESP implements the method for resp.Unmarshaler.
//
// UnmarshalRESP requires the value of I to be a pointer or nil. If I is nil
// then the RESP message will be read and discarded.
//
// UnmarshalRESP supports all go types supported by MarshalRESP, but has more
// flexibility. For example a RESP number message can be unmarshaled into a go
// string, or a RESP array with an even number of elements can be unmarshaled
// into a go map. There are some caveats:
//
// - If I is a resp.Unmarshaler then the resp.Unmarshaler will be unmarshaled
// into. If any element type of an aggregated type (e.g. array) is a
// resp.Unmarshaler then the same applies for each element being unmarshaled.
//
// - resp.SimpleError or resp.BlobError will be returned as the error from
// UnmarshalRESP when either message type is read. I will not be touched in this
// case.
//
// - RESP2 null bulk string and null bulk array messages are supported and
// are treated as null messages.
//
// - If I is an io.Writer then the RESP message's value will be written into it,
// encoded as if I were a []byte.
//
// - Streamed aggregated RESP messages will be treated as if they were their
// non-streamed counterpart, e.g. streamed arrays will be treated as arrays.
//
func (a Any) UnmarshalRESP(br *bufio.Reader) error {
	// if I is itself an Unmarshaler just hit that directly
	if u, ok := a.I.(resp.Unmarshaler); ok {
		return u.UnmarshalRESP(br)
	}

	b, err := br.Peek(1)
	if err != nil {
		return err
	}
	prefix := Prefix(b[0])

	// if the prefix is one of the error types then just parse and return that
	// full message here using the actual unmarshalers, which is easier than
	// re-implementing them.
	switch prefix {
	case SimpleErrorPrefix:
		var into SimpleError
		if err := into.UnmarshalRESP(br); err != nil {
			return err
		}
		return resp.ErrConnUsable{Err: into}
	case BlobErrorPrefix:
		var into BlobError
		if err := into.UnmarshalRESP(br); err != nil {
			return err
		}
		return resp.ErrConnUsable{Err: into}
	case AttributeHeaderPrefix:
		if err := discardAttribute(br); err != nil {
			return err
		}
		return a.UnmarshalRESP(br)
	}

	// This is a super special case that _must_ be handled before we actually
	// read from the reader. If an *interface{} is given we instead unmarshal
	// into a default (created based on the type of th message), then set the
	// *interface{} to that
	if ai, ok := a.I.(*interface{}); ok {

		// null is a special case of a special case. Just set ai to nil and
		// discard it, it's hard to handle it via saneDefault.
		if prefix == NullPrefix {
			*ai = nil
			return a.cp(nil).UnmarshalRESP(br)
		}

		def, err := saneDefault(prefix)
		if err != nil {
			return resp.ErrConnUsable{Err: err}
		}
		innerA := a.cp(def)
		if err := innerA.UnmarshalRESP(br); err != nil {
			return err
		}
		*ai = reflect.ValueOf(innerA.I).Elem().Interface()
		return nil
	}

	br.Discard(1)
	b, err = bytesutil.ReadBytesDelim(br)
	if err != nil {
		return err
	}

	switch prefix {
	case NullPrefix:
		return a.unmarshalNil()

	case ArrayHeaderPrefix, MapPrefix, SetHeaderPrefix, PushHeaderPrefix:
		var l int64
		if len(b) == 1 && b[0] == '?' {
			l = -1
		} else if l, err = bytesutil.ParseInt(b); err != nil {
			return err
		} else if l == -1 {
			return a.unmarshalNil()
		}
		return a.unmarshalAgg(prefix, br, l)

	case BlobStringPrefix, VerbatimStringPrefix:
		var l int64
		if len(b) == 1 && b[0] == '?' {
			l = -1
		} else if l, err = bytesutil.ParseInt(b); err != nil {
			return err
		} else if l == -1 {
			return a.unmarshalNil()
		}

		// if it's a verbatim string then discard the preceding type indicator
		// which is part of it.
		if prefix == VerbatimStringPrefix {
			if l < 4 {
				return errors.New("malformed verbatim string, invalid length")
			} else if _, err := br.Discard(4); err != nil {
				return err
			}
			l -= 4

		} else if l == -1 { // streamed string
			var buf *bytes.Buffer
			var r io.Reader = br
			w, ok := a.I.(io.Writer)
			if !ok {
				// If you're reading this comment and don't want to incur an
				// allocation here then pass in your own io.Writer as the I
				// field.
				buf = new(bytes.Buffer)
				r = buf
				w = buf
			}

			sw := StreamedStringReader{W: w}
			if err := sw.UnmarshalRESP(br); err != nil {
				return err
			} else if ok {
				return nil
			} else {
				return a.unmarshalSingle(r, buf.Len())
			}
		}

		// This is a bit of a clusterfuck. Basically:
		// - If unmarshal returns a non-ErrConnUsable error, return that asap.
		// - If discarding the last 2 bytes (in order to discard the full
		//   message) fails, return that asap
		// - Otherwise return the original error, if there was any
		if err = a.unmarshalSingle(br, int(l)); err != nil {
			if !errors.As(err, new(resp.ErrConnUsable)) {
				return err
			}
		}
		if _, discardErr := br.Discard(len(delim)); discardErr != nil {
			return discardErr
		}
		return err

	case BooleanPrefix:
		// convert the f/t boolean body to 0/1, so we're able to have consistent
		// logic related to unmarshaling booleans into ints/floats and
		// unmarshaling ints/floats/strings into booleans.
		if len(b) != 1 {
			return fmt.Errorf("malformed boolean resp body: %q", b)
		} else if b[0] == 't' {
			b[0] = '1'
		} else if b[0] == 'f' {
			b[0] = '0'
		} else {
			return fmt.Errorf("malformed boolean resp body: %q", b)
		}
		fallthrough

	case SimpleStringPrefix, NumberPrefix, DoublePrefix, BigNumberPrefix:
		reader := byteReaderPool.Get().(*bytes.Reader)
		reader.Reset(b)
		err := a.unmarshalSingle(reader, reader.Len())
		byteReaderPool.Put(reader)
		return err

	default:
		return fmt.Errorf("unknown type prefix %q", prefix)
	}
}

func (a Any) unmarshalSingle(body io.Reader, n int) error {
	var (
		err error
		i   int64
		ui  uint64
	)

	switch ai := a.I.(type) {
	case nil:
		// just read it and do nothing. This only catches the case of a.I being
		// actually nil, not a typed nil pointer.
		err = bytesutil.ReadNDiscard(body, n)
	case *[]byte:
		if *ai == nil {
			*ai = []byte{}
		}
		*ai, err = bytesutil.ReadNAppend(body, (*ai)[:0], n)
	case *string:
		scratch := bytesutil.GetBytes()
		defer bytesutil.PutBytes(scratch)
		*scratch, err = bytesutil.ReadNAppend(body, *scratch, n)
		*ai = string(*scratch)
	case *[]rune:
		if *ai == nil {
			*ai = []rune{}
		}
		scratch := bytesutil.GetBytes()
		defer bytesutil.PutBytes(scratch)
		*scratch, err = bytesutil.ReadNAppend(body, *scratch, n)
		*ai = []rune(string(*scratch))
	case *bool:
		var f float64
		f, err = bytesutil.ReadFloat(body, 64, n)
		*ai = f != 0
		break
	case *int:
		i, err = bytesutil.ReadInt(body, n)
		*ai = int(i)
	case *int8:
		i, err = bytesutil.ReadInt(body, n)
		*ai = int8(i)
	case *int16:
		i, err = bytesutil.ReadInt(body, n)
		*ai = int16(i)
	case *int32:
		i, err = bytesutil.ReadInt(body, n)
		*ai = int32(i)
	case *int64:
		i, err = bytesutil.ReadInt(body, n)
		*ai = i
	case *uint:
		ui, err = bytesutil.ReadUint(body, n)
		*ai = uint(ui)
	case *uint8:
		ui, err = bytesutil.ReadUint(body, n)
		*ai = uint8(ui)
	case *uint16:
		ui, err = bytesutil.ReadUint(body, n)
		*ai = uint16(ui)
	case *uint32:
		ui, err = bytesutil.ReadUint(body, n)
		*ai = uint32(ui)
	case *uint64:
		ui, err = bytesutil.ReadUint(body, n)
		*ai = ui
	case *float32:
		var f float64
		f, err = bytesutil.ReadFloat(body, 32, n)
		*ai = float32(f)
	case *float64:
		*ai, err = bytesutil.ReadFloat(body, 64, n)
	case io.Writer:
		_, err = io.CopyN(ai, body, int64(n))
	case encoding.TextUnmarshaler:
		scratch := bytesutil.GetBytes()
		defer bytesutil.PutBytes(scratch)
		if *scratch, err = bytesutil.ReadNAppend(body, *scratch, n); err != nil {
			break
		}
		err = ai.UnmarshalText(*scratch)
	case encoding.BinaryUnmarshaler:
		scratch := bytesutil.GetBytes()
		defer bytesutil.PutBytes(scratch)
		if *scratch, err = bytesutil.ReadNAppend(body, *scratch, n); err != nil {
			break
		}
		err = ai.UnmarshalBinary(*scratch)
	default:

		discardAndErr := func(fmtStr string, args ...interface{}) {
			scratch := bytesutil.GetBytes()
			defer bytesutil.PutBytes(scratch)
			if *scratch, err = bytesutil.ReadNAppend(body, *scratch, n); err != nil {
				return
			}
			err = fmt.Errorf(fmtStr+", message body was %q", append(args, *scratch)...)
			err = resp.ErrConnUsable{Err: err}
		}

		// check if the receiver is a non-nil pointer to a pointer, and if so
		// unmarshal into _that_, possibly filling in the inner pointer if it's
		// nil.
		if ptr := reflect.ValueOf(a.I); ptr.Kind() == reflect.Ptr {
			if ptr.IsNil() {
				discardAndErr("can't unmarshal into nil %s", ptr.Type())
				break
			} else if innerPtr := ptr.Elem(); innerPtr.Kind() == reflect.Ptr {
				if innerPtr.IsNil() {
					innerPtr.Set(reflect.New(innerPtr.Type().Elem()))
				}
				return a.cp(innerPtr.Interface()).unmarshalSingle(body, n)
			}
		}

		discardAndErr("can't unmarshal into non-pointer %T", a.I)
	}

	return err
}

func (a Any) unmarshalNil() error {
	vv := reflect.ValueOf(a.I)
	if vv.Kind() != reflect.Ptr || !vv.Elem().CanSet() {
		// If the type in I can't be set then just ignore it. This is kind of
		// weird but it's what encoding/json does in the same circumstance
		return nil
	}

	vve := vv.Elem()
	vve.Set(reflect.Zero(vve.Type()))
	return nil
}

func (a Any) maybeUnmarshalRESP(br *bufio.Reader, stream bool) (bool, error) {
	if !stream {
		return true, a.UnmarshalRESP(br)
	}

	// TODO this might cause Any to end up on the heap
	streamAgg := StreamedAggregatedElement{Unmarshaler: &a}
	err := streamAgg.UnmarshalRESP(br)
	return !streamAgg.End, err
}

var interfacePtrType = reflect.TypeOf(new(interface{}))

// keyableReceiver checks if kv is a *interface{} and if so ensures that the
// type it will end up being is allowed to be a map key. It returns the
// reflect.Value to unmarshal into, or an error.
//
// Since the reflect.Value returned might not be the original kv,
// kv.Elem().Set(result.Elem()) should be called after unmarshaling is complete.
func keyableReceiver(prefix Prefix, kv reflect.Value) (reflect.Value, error) {
	if kv.Type() != interfacePtrType {
		return kv, nil
	}

	into, err := saneDefault(prefix)
	if err != nil {
		return reflect.Value{}, err
	}
	intoV := reflect.ValueOf(into)

	switch intoV.Elem().Kind() {
	case reflect.Slice, reflect.Map, reflect.Func:
		err := fmt.Errorf("resp message of type %s would get unmarshaled as type %s, but that type can't be a map key", prefix, intoV.Elem().Type())
		return reflect.Value{}, resp.ErrConnUsable{Err: err}
	}

	return intoV, nil
}

func (a Any) unmarshalAgg(prefix Prefix, br *bufio.Reader, l int64) error {
	if prefix == MapPrefix {
		l *= 2
	}

	size := int(l)
	stream := size < 0
	if a.I == nil {
		return discardMulti(br, size)
	}

	v := reflect.ValueOf(a.I)
	if v.Kind() != reflect.Ptr {
		err := resp.ErrConnUsable{
			Err: fmt.Errorf("can't unmarshal resp %s into %T", prefix, a.I),
		}
		return discardMultiAfterErr(br, size, err)
	}

	for ; v.Kind() == reflect.Ptr; v = reflect.Indirect(v) {
		// this loop de-references as many pointers as possible.
	}

	switch v.Kind() {
	case reflect.Slice:
		slice := v
		if size > slice.Cap() || slice.IsNil() {
			sliceSize := size
			if stream {
				sliceSize = 8
			}
			slice.Set(reflect.MakeSlice(slice.Type(), 0, sliceSize))
		} else {
			slice.SetLen(0)
		}

		// TODO this isn't ideal, but it works for now. Ideally this loop would
		// be unmarshaling straight into slice elements based on i, expanding
		// slice as needed, before finally setting the length appropriately at
		// the end.
		into := reflect.New(v.Type().Elem())
		for i := 0; i < size || stream; i++ {
			into.Elem().Set(reflect.Zero(into.Type().Elem()))
			if more, err := a.cp(into.Interface()).maybeUnmarshalRESP(br, stream); err != nil {
				return discardMultiAfterErr(br, size-i-1, err)
			} else if !more {
				break
			}
			slice = reflect.Append(slice, into.Elem())
		}
		v.Set(slice)
		return nil

	case reflect.Map:
		setMap := isSetMap(v.Type())
		if !stream && !setMap && size%2 != 0 {
			err := resp.ErrConnUsable{Err: fmt.Errorf("cannot decode resp %s with odd number of elements into map", prefix)}
			return discardMultiAfterErr(br, size, err)
		} else if v.IsNil() {
			mapSize := size
			if stream {
				mapSize = 3
			}
			v.Set(reflect.MakeMapWithSize(v.Type(), mapSize))
		} else {
			for _, key := range v.MapKeys() {
				v.SetMapIndex(key, reflect.Value{})
			}
		}

		kt := v.Type().Key()
		var kvs reflect.Value
		if size > 0 && canShareReflectValue(kt) {
			kvs = reflect.New(v.Type().Key())
		}

		vt := v.Type().Elem()
		var vvs reflect.Value
		if setMap {
			vvs = reflect.New(emptyStructT)
		} else if size > 0 && canShareReflectValue(vt) {
			vvs = reflect.New(vt)
		}

		incr := 2
		if setMap {
			incr = 1
		}

		for i := 0; i < size || stream; i += incr {
			kv := kvs
			if !kv.IsValid() {
				kv = reflect.New(kt)
			}

			// we use keyableReceiver to ensure that, if kt is interface{}, the
			// value which is going to end up being received into can actually
			// be a map key. If the next message is a
			// StreamedAggregatedTypeEndPrefix then it doesn't really matter
			// because nothing will actually be unmarshaled, so skip the check
			// in that case.
			krcv := kv
			if b, err := br.Peek(1); err != nil {
				return err
			} else if prefix := Prefix(b[0]); !stream || prefix != StreamedAggregatedTypeEndPrefix {
				krcv, err = keyableReceiver(prefix, kv)
				if err != nil {
					return err
				}
			}

			if more, err := (a.cp(krcv.Interface())).maybeUnmarshalRESP(br, stream); err != nil {
				return discardMultiAfterErr(br, size-i-1, err)
			} else if !more {
				break
			}

			// see keyableReceiver for why this is happening
			kv.Elem().Set(krcv.Elem())

			vv := vvs
			if !setMap {
				if !vv.IsValid() {
					vv = reflect.New(vt)
				}
				if err := (a.cp(vv.Interface())).UnmarshalRESP(br); err != nil {
					return discardMultiAfterErr(br, int(l)-i-2, err)
				}
			}

			v.SetMapIndex(kv.Elem(), vv.Elem())
		}
		return nil

	case reflect.Struct:
		if !stream && size%2 != 0 {
			err := resp.ErrConnUsable{Err: fmt.Errorf("cannot decode resp %s with odd number of elements into struct", prefix)}
			return discardMultiAfterErr(br, size, err)
		}

		structFields := getStructFields(v.Type())
		var field string

		for i := 0; i < size || stream; i += 2 {
			if more, err := a.cp(&field).maybeUnmarshalRESP(br, stream); err != nil {
				return discardMultiAfterErr(br, size-i-1, err)
			} else if !more {
				break
			}

			var vv reflect.Value
			structField, ok := structFields[field]
			if ok {
				vv = getStructField(v, structField.indices)
			}

			if !ok || !vv.IsValid() {
				// discard the value
				if err := a.cp(nil).UnmarshalRESP(br); err != nil {
					return discardMultiAfterErr(br, size-i-2, err)
				}
				continue
			}

			if err := a.cp(vv.Interface()).UnmarshalRESP(br); err != nil {
				return discardMultiAfterErr(br, size-i-2, err)
			}
		}

		return nil

	default:
		err := resp.ErrConnUsable{Err: fmt.Errorf("cannot decode resp %s into %v", prefix, v.Type())}
		return discardMultiAfterErr(br, int(l), err)
	}
}

func canShareReflectValue(ty reflect.Type) bool {
	switch ty.Kind() {
	case reflect.Bool,
		reflect.Int,
		reflect.Int8,
		reflect.Int16,
		reflect.Int32,
		reflect.Int64,
		reflect.Uint,
		reflect.Uint8,
		reflect.Uint16,
		reflect.Uint32,
		reflect.Uint64,
		reflect.Uintptr,
		reflect.Float32,
		reflect.Float64,
		reflect.Complex64,
		reflect.Complex128,
		reflect.String:
		return true
	default:
		return false
	}
}

type structField struct {
	name    string
	fromTag bool // from a tag overwrites a field name
	indices []int
}

// encoding/json uses a similar pattern for unmarshaling into structs
var structFieldsCache sync.Map // aka map[reflect.Type]map[string]structField

func getStructFields(t reflect.Type) map[string]structField {
	if mV, ok := structFieldsCache.Load(t); ok {
		return mV.(map[string]structField)
	}

	getIndices := func(parents []int, i int) []int {
		indices := make([]int, len(parents), len(parents)+1)
		copy(indices, parents)
		indices = append(indices, i)
		return indices
	}

	m := map[string]structField{}

	var populateFrom func(reflect.Type, []int)
	populateFrom = func(t reflect.Type, parents []int) {
		if t.Kind() == reflect.Ptr {
			t = t.Elem()
		}
		l := t.NumField()

		// first get all fields which aren't embedded structs
		for i := 0; i < l; i++ {
			ft := t.Field(i)
			if ft.Anonymous || ft.PkgPath != "" {
				continue
			}

			key, fromTag := ft.Name, false
			if tag := ft.Tag.Get("redis"); tag != "" && tag != "-" {
				key, fromTag = tag, true
			}
			if m[key].fromTag {
				continue
			}
			m[key] = structField{
				name:    key,
				fromTag: fromTag,
				indices: getIndices(parents, i),
			}
		}

		// then find all embedded structs and descend into them
		for i := 0; i < l; i++ {
			ft := t.Field(i)
			if !ft.Anonymous {
				continue
			}
			populateFrom(ft.Type, getIndices(parents, i))
		}
	}

	populateFrom(t, []int{})
	structFieldsCache.LoadOrStore(t, m)
	return m
}

// v must be setable. Always returns a Kind() == reflect.Ptr, unless it returns
// the zero Value, which means a setable value couldn't be gotten.
func getStructField(v reflect.Value, ii []int) reflect.Value {
	if len(ii) == 0 {
		return v.Addr()
	}
	i, ii := ii[0], ii[1:]

	iv := v.Field(i)
	if iv.Kind() == reflect.Ptr && iv.IsNil() {
		// If the field is a pointer to an unexported type then it won't be
		// settable, though if the user pre-sets the value it will be (I think).
		if !iv.CanSet() {
			return reflect.Value{}
		}
		iv.Set(reflect.New(iv.Type().Elem()))
	}
	iv = reflect.Indirect(iv)

	return getStructField(iv, ii)
}

////////////////////////////////////////////////////////////////////////////////

// RawMessage is a raw encoded RESP message. When marshaling the exact bytes of
// the RawMessage will be written as-is. When unmarshaling the bytes of a single
// RESP message will be read into the RawMessage's bytes.
type RawMessage []byte

// MarshalRESP implements the method for resp.Marshaler.
func (rm RawMessage) MarshalRESP(w io.Writer) error {
	_, err := w.Write(rm)
	return err
}

// UnmarshalRESP implements the method for resp.Unmarshaler.
func (rm *RawMessage) UnmarshalRESP(br *bufio.Reader) error {
	*rm = (*rm)[:0]
	return rm.unmarshal(br)
}

func (rm *RawMessage) unmarshal(br *bufio.Reader) error {
	b, err := br.ReadSlice('\n')
	if err != nil {
		return err
	}
	*rm = append(*rm, b...)

	if len(b) < 3 {
		return errors.New("malformed data read")
	}
	body := b[1 : len(b)-2]

	prefix := Prefix(b[0])
	switch prefix {
	case ArrayHeaderPrefix, SetHeaderPrefix, MapPrefix, PushHeaderPrefix, AttributeHeaderPrefix:
		if body[0] == '?' {
			return nil
		}

		l, err := bytesutil.ParseInt(body)
		if err != nil {
			return err
		} else if l == -1 {
			return nil
		}

		switch prefix {
		case MapPrefix, AttributeHeaderPrefix:
			l *= 2
		}

		for i := 0; i < int(l); i++ {
			if err := rm.unmarshal(br); err != nil {
				return err
			}
		}
		return nil
	case BlobStringPrefix, VerbatimStringPrefix, BlobErrorPrefix, StreamedStringChunkPrefix:
		if prefix == BlobStringPrefix && body[0] == '?' {
			return nil
		}

		l, err := bytesutil.ParseInt(body) // fuck DRY
		if err != nil {
			return err
		} else if l == -1 {
			return nil
		} else if prefix == StreamedStringChunkPrefix && l == 0 {
			return nil
		}

		*rm, err = bytesutil.ReadNAppend(br, *rm, int(l+2))
		return err
	case SimpleErrorPrefix, SimpleStringPrefix, NumberPrefix, DoublePrefix, BigNumberPrefix, StreamedAggregatedTypeEndPrefix, NullPrefix, BooleanPrefix:
		return nil
	default:
		return fmt.Errorf("unknown type prefix %q", b[0])
	}
}

// UnmarshalInto is a shortcut for wrapping this RawMessage in a *bufio.Reader
// and passing that into the given Unmarshaler's UnmarshalRESP method. Any error
// from calling UnmarshalRESP is returned, and the RawMessage is unaffected in
// all cases.
func (rm RawMessage) UnmarshalInto(u resp.Unmarshaler) error {
	r := byteReaderPool.Get().(*bytes.Reader)
	defer byteReaderPool.Put(r)
	r.Reset(rm)

	br := bufioReaderPool.Get().(*bufio.Reader)
	defer bufioReaderPool.Put(br)
	br.Reset(r)

	return u.UnmarshalRESP(br)
}

// IsNull returns true if the contents of the RawMessage is a null RESP3
// message, or a RESP2 bulk/array null message.
func (rm RawMessage) IsNull() bool {
	if bytes.Equal(rm, null) {
		return true
	} else if len(rm) < len(nullRESP2Suffix)+1 {
		return false
	}
	return bytes.Equal(rm[1:len(nullRESP2Suffix)+1], nullRESP2Suffix)
}

// IsEmpty returns true if the RawMessage is an aggregated type with zero
// elements.
func (rm RawMessage) IsEmpty() bool {
	if len(rm) == 0 {
		return false
	}
	switch Prefix(rm[0]) {
	case ArrayHeaderPrefix, MapPrefix, SetHeaderPrefix, AttributeHeaderPrefix, PushHeaderPrefix:
		return bytes.Equal(rm[1:], emptyAggTail)
	}
	return false
}

// IsStreamedHeader returns true if the RawMessage is the header of a streamed
// aggregated type or a streamed string.
func (rm RawMessage) IsStreamedHeader() bool {
	if len(rm) == 0 {
		return false
	}
	switch Prefix(rm[0]) {
	case ArrayHeaderPrefix, MapPrefix, SetHeaderPrefix, AttributeHeaderPrefix, PushHeaderPrefix, BlobStringPrefix:
		return bytes.Equal(rm[1:], streamedHeaderTail)
	}
	return false
}

////////////////////////////////////////////////////////////////////////////////

// rawMsgs is like RawMessage except that it can contain one or more individual
// RESP messages.
type rawMsgs []byte

func (rm rawMsgs) next() (RawMessage, rawMsgs, error) {
	if len(rm) == 0 {
		return nil, nil, io.EOF
	}

	i := bytes.Index(rm, delim)
	if i < 0 {
		return nil, nil, fmt.Errorf("malformed RESP message %q", rm)
	}

	prefix := Prefix(rm[0])
	switch prefix {
	case BlobStringPrefix, BlobErrorPrefix, VerbatimStringPrefix,
		StreamedStringChunkPrefix:
		body := rm[1:i]
		l, err := bytesutil.ParseInt(body)
		if err != nil {
			return nil, nil, fmt.Errorf("could not parse %q as int64: %v", body, err)
		} else if l >= 0 {
			i += int(l) + len(delim)
		}
	}

	return RawMessage(rm[:i+len(delim)]), rm[i+len(delim):], nil
}

func (rm rawMsgs) numElems() (int, error) {
	var n int
	for {
		var body RawMessage
		var err error
		body, rm, err = rm.next()
		if errors.Is(err, io.EOF) {
			return n, nil
		} else if err != nil {
			return 0, err
		}

		prefix := Prefix(body[0])
		if prefix == BlobErrorPrefix && body.IsStreamedHeader() {
			return 0, errors.New("cannot determine number of elements of streamed blob string")
		} else if body.IsNull() {
			n++
			continue
		}

		switch prefix {
		case BlobStringPrefix, SimpleStringPrefix, SimpleErrorPrefix,
			NumberPrefix, NullPrefix, DoublePrefix, BooleanPrefix,
			BlobErrorPrefix, VerbatimStringPrefix, BigNumberPrefix:
			n++
		case ArrayHeaderPrefix, MapPrefix, SetHeaderPrefix,
			AttributeHeaderPrefix, PushHeaderPrefix:
			// no increment
		default:
			return 0, fmt.Errorf("cannot determine number of elements in RawMessage with prefix %v", prefix)
		}
	}
}
