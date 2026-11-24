package ankiimport

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"
)

const maxProtoFields = 100_000

type protoField struct {
	number int
	wire   int
	value  uint64
	bytes  []byte
}

func decodeProto(data []byte) ([]protoField, error) {
	fields := make([]protoField, 0)
	for offset := 0; offset < len(data); {
		key, next, err := readVarint(data, offset)
		if err != nil {
			return nil, err
		}
		offset = next
		field := protoField{number: int(key >> 3), wire: int(key & 7)}
		if field.number <= 0 {
			return nil, errors.New("invalid protobuf field number")
		}
		switch field.wire {
		case 0:
			field.value, offset, err = readVarint(data, offset)
		case 1:
			if len(data)-offset < 8 {
				err = errors.New("truncated protobuf fixed64 field")
			} else {
				field.bytes = data[offset : offset+8]
				offset += 8
			}
		case 2:
			var length uint64
			length, offset, err = readVarint(data, offset)
			if err == nil {
				if length > uint64(len(data)-offset) {
					err = errors.New("truncated protobuf bytes field")
				} else {
					field.bytes = data[offset : offset+int(length)]
					offset += int(length)
				}
			}
		case 5:
			if len(data)-offset < 4 {
				err = errors.New("truncated protobuf fixed32 field")
			} else {
				field.bytes = data[offset : offset+4]
				offset += 4
			}
		default:
			err = fmt.Errorf("unsupported protobuf wire type %d", field.wire)
		}
		if err != nil {
			return nil, err
		}
		if len(fields) >= maxProtoFields {
			return nil, errors.New("protobuf contains too many fields")
		}
		fields = append(fields, field)
	}
	return fields, nil
}

func readVarint(data []byte, offset int) (uint64, int, error) {
	var value uint64
	for shift := 0; offset < len(data) && shift < 64; shift += 7 {
		current := data[offset]
		offset++
		value |= uint64(current&0x7f) << shift
		if current&0x80 == 0 {
			return value, offset, nil
		}
	}
	return 0, offset, errors.New("truncated or oversized protobuf varint")
}

func fieldBytes(fields []protoField, number int) []byte {
	for _, field := range fields {
		if field.number == number && field.wire == 2 {
			return field.bytes
		}
	}
	return nil
}

func fieldString(fields []protoField, number int) (string, error) {
	value := fieldBytes(fields, number)
	if value == nil {
		return "", nil
	}
	if !utf8.Valid(value) {
		return "", errors.New("protobuf text is not valid UTF-8")
	}
	return string(value), nil
}

func fieldInt(fields []protoField, number int) (int64, bool) {
	for _, field := range fields {
		if field.number == number && field.wire == 0 && field.value <= math.MaxInt64 {
			return int64(field.value), true
		}
	}
	return 0, false
}

func fixed32Float(field protoField) (float32, bool) {
	if field.wire != 5 || len(field.bytes) != 4 {
		return 0, false
	}
	return math.Float32frombits(binary.LittleEndian.Uint32(field.bytes)), true
}
