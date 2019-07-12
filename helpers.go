package go_parquet

import (
	"io"

	"github.com/apache/thrift/lib/go/thrift"
	"github.com/pkg/errors"
)

type byteReader struct {
	io.Reader
}

func (br *byteReader) ReadByte() (byte, error) {
	buf := make([]byte, 1)
	if _, err := io.ReadFull(br.Reader, buf); err != nil {
		return 0, err
	}

	return buf[0], nil
}

type offsetReader struct {
	inner  io.ReadSeeker
	offset int64
	count  int64
}

func (o *offsetReader) Read(p []byte) (int, error) {
	n, err := o.inner.Read(p)
	o.offset += int64(n)
	o.count += int64(n)
	return n, err
}

func (o *offsetReader) Seek(offset int64, whence int) (int64, error) {
	i, err := o.inner.Seek(offset, whence)
	if err == nil {
		o.count += i - o.offset
		o.offset = i
	}

	return i, err
}

func (o *offsetReader) Count() int64 {
	return o.count
}

func decodeRLEValue(bytes []byte) int32 {
	switch len(bytes) {
	case 1:
		return int32(bytes[0])
	case 2:
		return int32(bytes[0]) + int32(bytes[1])<<8
	case 3:
		return int32(bytes[0]) + int32(bytes[1])<<8 + int32(bytes[2])<<16
	case 4:
		return int32(bytes[0]) + int32(bytes[1])<<8 + int32(bytes[2])<<16 + int32(bytes[3])<<24
	default:
		panic("invalid argument")
	}
}

func encodeRLEValue(in int32, size int) []byte {
	switch size {
	case 1:
		return []byte{byte(in & 255)}
	case 2:
		return []byte{
			byte(in & 255),
			byte((in >> 8) & 255),
		}
	case 3:
		return []byte{
			byte(in & 255),
			byte((in >> 8) & 255),
			byte((in >> 16) & 255),
		}
	case 4:
		return []byte{
			byte(in & 255),
			byte((in >> 8) & 255),
			byte((in >> 16) & 255),
			byte((in >> 24) & 255),
		}
	default:
		panic("invalid argument")
	}
}

func writeFull(w io.Writer, buf []byte) error {
	cnt, err := w.Write(buf)
	if err != nil {
		return err
	}

	if cnt != len(buf) {
		return errors.Errorf("need to write %d byte wrote %d", cnt, len(buf))
	}

	return nil
}

type thriftReader interface {
	Read(thrift.TProtocol) error
}

func readThrift(tr thriftReader, r io.Reader) error {
	// Make sure we are not using any kind of buffered reader here. bufio.Reader "can" reads more data ahead of time,
	// which is a problem on this library
	transport := &thrift.StreamTransport{Reader: r}
	proto := thrift.NewTCompactProtocol(transport)
	return tr.Read(proto)
}

func repeat(i int32, count int) []int32 {
	ret := make([]int32, count)
	for j := range ret {
		ret[j] = i
	}

	return ret
}

func decodeLevels(d decoder, ctx *hybridContext, data []uint16) error {
	for i := range data {
		u, err := d.next(ctx)
		if err != nil {
			return err
		}
		data[i] = uint16(u)
	}

	return nil
}