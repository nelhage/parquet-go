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
	transport := thrift.NewStreamTransportR(r)
	proto := thrift.NewTCompactProtocol(transport)
	return tr.Read(proto)
}
