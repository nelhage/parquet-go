package go_parquet

import (
	"bytes"
	"io"
	"strings"

	"github.com/pkg/errors"
	"github.com/fraugster/parquet-go/parquet"
)

type dataPageReaderV2 struct {
	ph *parquet.PageHeader

	valuesCount        int32
	encoding           parquet.Encoding
	valuesDecoder      valuesDecoder
	dDecoder, rDecoder levelDecoder
	fn                 getValueDecoderFn
	position           int
}

func (dp *dataPageReaderV2) numValues() int32 {
	return dp.valuesCount
}

func (dp *dataPageReaderV2) readValues(val []interface{}) (n int, dLevel []int32, rLevel []int32, err error) {
	size := len(val)
	if rem := int(dp.valuesCount) - dp.position; rem < size {
		size = rem
	}

	if size == 0 {
		return 0, nil, nil, nil
	}

	dLevel = make([]int32, size)
	if err := decodeInt32(dp.dDecoder, dLevel); err != nil {
		return 0, nil, nil, errors.Wrap(err, "read definition levels failed")
	}

	rLevel = make([]int32, size)
	if err := decodeInt32(dp.rDecoder, rLevel); err != nil {
		return 0, nil, nil, errors.Wrap(err, "read repetition levels failed")
	}

	notNull := 0
	for _, dl := range dLevel {
		if dl == int32(dp.dDecoder.maxLevel()) {
			notNull++
		}
	}

	if notNull != 0 {
		if n, err := dp.valuesDecoder.decodeValues(val[:notNull]); err != nil {
			return 0, nil, nil, errors.Wrapf(err, "read values from page failed, need %d values but read %d", notNull, n)
		}
	}
	dp.position += size
	return size, dLevel, rLevel, nil
}

func (dp *dataPageReaderV2) init(dDecoder, rDecoder getLevelDecoder, values getValueDecoderFn) error {
	var err error
	// Page v2 dose not have any encoding for the levels
	dp.dDecoder, err = dDecoder(parquet.Encoding_RLE)
	if err != nil {
		return err
	}
	dp.rDecoder, err = rDecoder(parquet.Encoding_RLE)
	if err != nil {
		return err
	}
	dp.fn = values
	dp.position = 0

	return nil
}

func (dp *dataPageReaderV2) read(r io.ReadSeeker, ph *parquet.PageHeader, codec parquet.CompressionCodec) (err error) {
	// TODO: verify this format, there is some question
	// 1- Uncompressed size is affected by the level lens?
	// 2- If the levels are actually rle and the first byte is the size, since there is already size in header (NO)
	if ph.DataPageHeaderV2 == nil {
		return errors.Errorf("null DataPageHeaderV2 in %+v", ph)
	}

	if dp.valuesCount = ph.DataPageHeaderV2.NumValues; dp.valuesCount < 0 {
		return errors.Errorf("negative NumValues in DATA_PAGE_V2: %d", dp.valuesCount)
	}

	if ph.DataPageHeaderV2.RepetitionLevelsByteLength < 0 {
		return errors.Errorf("invalid RepetitionLevelsByteLength")
	}
	if ph.DataPageHeaderV2.DefinitionLevelsByteLength < 0 {
		return errors.Errorf("invalid DefinitionLevelsByteLength")
	}
	dp.encoding = ph.DataPageHeader.Encoding
	dp.ph = ph

	if dp.valuesDecoder, err = dp.fn(dp.encoding); err != nil {
		return err
	}

	// Its safe to call this {r,d}Decoder later, since the stream they operate on are in memory
	levelsSize := ph.DataPageHeaderV2.RepetitionLevelsByteLength + ph.DataPageHeaderV2.DefinitionLevelsByteLength
	// read both level size
	if levelsSize > 0 {
		data := make([]byte, levelsSize)
		n, err := io.ReadFull(r, data)
		if err != nil {
			return errors.Wrapf(err, "need to read %d byte but there was only %d byte", levelsSize, n)
		}

		// In this image https://camo.githubusercontent.com/0f0b52f7405720585ed7303c9ff317f272ebba19/68747470733a2f2f7261772e6769746875622e636f6d2f6170616368652f706172717565742d666f726d61742f6d61737465722f646f632f696d616765732f46696c654c61796f75742e676966
		// the repetition is before definition, but page v1 is different TODO: verify this
		if ph.DataPageHeaderV2.DefinitionLevelsByteLength > 0 {
			if err := dp.dDecoder.init(bytes.NewReader(data[int(ph.DataPageHeaderV2.RepetitionLevelsByteLength):])); err != nil {
				return errors.Wrapf(err, "read definition level failed")
			}
		}

		if ph.DataPageHeaderV2.RepetitionLevelsByteLength > 0 {
			if err := dp.rDecoder.init(bytes.NewReader(data[:int(ph.DataPageHeaderV2.RepetitionLevelsByteLength)])); err != nil {
				return errors.Wrapf(err, "read repetition level failed")
			}
		}
	}

	// TODO: (F0rud) I am not sure if this is correct to subtract the level size from the compressed size here
	reader, err := createDataReader(r, codec, ph.GetCompressedPageSize()-levelsSize, ph.GetUncompressedPageSize()-levelsSize)
	if err != nil {
		return err
	}

	return dp.valuesDecoder.init(reader)
}

type dataPageWriterV2 struct {
	col    *column
	schema SchemaWriter

	codec      parquet.CompressionCodec
	dictionary bool
}

func (dp *dataPageWriterV2) init(schema SchemaWriter, col *column, codec parquet.CompressionCodec) error {
	dp.col = col
	dp.codec = codec
	dp.schema = schema
	return nil
}

func (dp *dataPageWriterV2) getHeader(comp, unComp, defSize, repSize int, isCompressed bool) *parquet.PageHeader {
	ph := &parquet.PageHeader{
		Type:                 parquet.PageType_DATA_PAGE,
		UncompressedPageSize: int32(unComp),
		CompressedPageSize:   int32(comp),
		Crc:                  nil, // TODO: add crc?
		DataPageHeaderV2: &parquet.DataPageHeaderV2{
			NumValues:                  dp.col.data.values.numValues(),
			NumNulls:                   dp.col.data.values.nullValueCount(),
			NumRows:                    int32(dp.schema.NumRecords()),
			Encoding:                   dp.col.data.encoding(),
			DefinitionLevelsByteLength: int32(defSize),
			RepetitionLevelsByteLength: int32(repSize),
			IsCompressed:               isCompressed,
			Statistics:                 nil,
		},
	}
	return ph
}

func (dp *dataPageWriterV2) write(w io.Writer) (int, int, error) {
	// In V2 data page is compressed separately
	nested := strings.IndexByte(dp.col.FlatName(), '.') >= 0

	def := &bytes.Buffer{}
	// if it is nested or it is not repeated we need the dLevel data
	if nested || dp.col.data.repetitionType() != parquet.FieldRepetitionType_REQUIRED {
		if err := encodeLevels(def, dp.col.MaxDefinitionLevel(), dp.col.data.dLevels); err != nil {
			return 0, 0, err
		}
	}

	rep := &bytes.Buffer{}
	// if this is nested or if the data is repeated
	if nested || dp.col.data.repetitionType() == parquet.FieldRepetitionType_REPEATED {
		if err := encodeLevels(rep, dp.col.MaxRepetitionLevel(), dp.col.data.rLevels); err != nil {
			return 0, 0, err
		}
	}

	dataBuf := &bytes.Buffer{}
	enc := dp.col.data.encoding()
	if dp.dictionary {
		enc = parquet.Encoding_RLE_DICTIONARY
	}

	encoder, err := getValuesEncoder(enc, dp.col.Element(), dp.col.data.values)
	if err != nil {
		return 0, 0, err
	}

	if err := encodeValue(dataBuf, encoder, dp.col.data.values.assemble(false)); err != nil {
		return 0, 0, err
	}

	comp, err := compressBlock(dataBuf.Bytes(), dp.codec)
	if err != nil {
		return 0, 0, errors.Wrapf(err, "compressing data failed with %s method", dp.codec)
	}
	compSize, unCompSize := len(comp), len(dataBuf.Bytes())
	defLen, repLen := def.Len(), rep.Len()
	header := dp.getHeader(compSize, unCompSize, defLen, repLen, dp.codec != parquet.CompressionCodec_UNCOMPRESSED)
	if err := writeThrift(header, w); err != nil {
		return 0, 0, err
	}

	if err := writeFull(w, def.Bytes()); err != nil {
		return 0, 0, err
	}

	if err := writeFull(w, rep.Bytes()); err != nil {
		return 0, 0, err
	}

	return compSize + defLen + repLen, unCompSize + defLen + repLen, writeFull(w, comp)
}