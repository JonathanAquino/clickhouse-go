package clickhouse

import (
	"bytes"
	"database/sql/driver"
	"fmt"
	"strings"
)

// data block
type block struct {
	table       string
	info        blockInfo
	numRows     uint64
	numColumns  uint64
	columnNames []string
	columnTypes []string
	columns     [][]interface{}
	offsets     [][]uint64
	buffers     []bytes.Buffer
}

type blockInfo struct {
	num1        uint64
	isOverflows bool
	num2        uint64
	bucketNum   int32
	num3        uint64
}

func (info *blockInfo) read(conn *connect) error {
	var err error
	if info.num1, err = readUvarint(conn); err != nil {
		return err
	}
	if info.isOverflows, err = readBool(conn); err != nil {
		return err
	}
	if info.num2, err = readUvarint(conn); err != nil {
		return err
	}
	if info.bucketNum, err = readInt32(conn); err != nil {
		return err
	}
	if info.num3, err = readUvarint(conn); err != nil {
		return err
	}
	return nil
}

func (info *blockInfo) write(conn *connect) error {
	if err := writeUvarint(conn, info.num1); err != nil {
		return err
	}
	if info.num1 != 0 {
		if err := writeBool(conn, info.isOverflows); err != nil {
			return err
		}
		if err := writeUvarint(conn, info.num2); err != nil {
			return err
		}
		if err := writeInt32(conn, info.bucketNum); err != nil {
			return err
		}
		if err := writeUvarint(conn, info.num3); err != nil {
			return err
		}
	}
	return nil
}

func (b *block) read(revision uint64, conn *connect) error {
	var err error
	if revision >= DBMS_MIN_REVISION_WITH_TEMPORARY_TABLES {
		if b.table, err = readString(conn); err != nil {
			return err
		}
	}
	if revision >= DBMS_MIN_REVISION_WITH_BLOCK_INFO {
		if err := b.info.read(conn); err != nil {
			return err
		}
	}
	if b.numColumns, err = readUvarint(conn); err != nil {
		return err
	}
	if b.numRows, err = readUvarint(conn); err != nil {
		return err
	}
	b.columns = make([][]interface{}, b.numColumns)
	for i := 0; i < int(b.numColumns); i++ {
		var column, columnType string
		if column, err = readString(conn); err != nil {
			return err
		}
		if columnType, err = readString(conn); err != nil {
			return err
		}
		b.columnNames = append(b.columnNames, column)
		b.columnTypes = append(b.columnTypes, columnType)
		switch {
		case strings.HasPrefix(columnType, "Array"):
			offsets := make([]uint64, 0, b.numRows)
			for row := 0; row < int(b.numRows); row++ {
				offset, err := readUInt64(conn)
				if err != nil {
					return err
				}
				offsets = append(offsets, offset)
			}
			for n, offset := range offsets {
				len := offset
				if n != 0 {
					len = len - offsets[n-1]
				}
				value, err := readArray(conn, columnType, len)
				if err != nil {
					return err
				}
				b.columns[i] = append(b.columns[i], value)
			}
		default:
			for row := 0; row < int(b.numRows); row++ {
				value, err := read(conn, columnType)
				if err != nil {
					return err
				}
				b.columns[i] = append(b.columns[i], value)
			}
		}
	}
	return nil
}

func (b *block) write(revision uint64, conn *connect) error {
	if err := writeUvarint(conn, ClientDataPacket); err != nil {
		return err
	}
	if revision >= DBMS_MIN_REVISION_WITH_TEMPORARY_TABLES {
		if err := writeString(conn, b.table); err != nil {
			return err
		}
	}
	if revision >= DBMS_MIN_REVISION_WITH_BLOCK_INFO {
		if err := b.info.write(conn); err != nil {
			return err
		}
	}
	if err := writeUvarint(conn, b.numColumns); err != nil {
		return err
	}
	if err := writeUvarint(conn, b.numRows); err != nil {
		return err
	}
	for i, column := range b.columnNames {
		columnType := b.columnTypes[i]
		if err := writeString(conn, column); err != nil {
			return err
		}
		if err := writeString(conn, columnType); err != nil {
			return err
		}
		for _, offset := range b.offsets[i] {
			if err := writeUInt64(conn, offset); err != nil {
				return err
			}
		}
		if _, err := b.buffers[i].WriteTo(conn); err != nil {
			return err
		}
	}
	return nil
}

func (b *block) append(args []driver.Value) error {
	if len(b.buffers) == 0 && len(args) != 0 {
		b.numRows = 0
		b.offsets = make([][]uint64, len(args))
		b.buffers = make([]bytes.Buffer, len(args))
	}
	b.numRows++
	for columnNum := range b.columnTypes {
		var (
			column     = b.columnNames[columnNum]
			columnType = b.columnTypes[columnNum]
			buffer     = &b.buffers[columnNum]
		)
		switch {
		case strings.HasPrefix(columnType, "Array"):
			array, ok := args[columnNum].([]byte)
			if !ok {
				return fmt.Errorf("Column %s (%s): unexpected type %T of value", column, columnType, args[columnNum])
			}
			ct, arrayLen, data, err := arrayInfo(array)
			if err != nil {
				return err
			}
			if len(b.offsets[columnNum]) == 0 {
				b.offsets[columnNum] = append(b.offsets[columnNum], arrayLen)
			} else {
				b.offsets[columnNum] = append(b.offsets[columnNum], arrayLen+b.offsets[columnNum][len(b.offsets[columnNum])-1])
			}
			if "Array("+ct+")" != columnType {
				return fmt.Errorf("Column %s (%s): unexpected type %s of value", column, columnType, ct)
			}
			if _, err := buffer.Write(data); err != nil {
				return err
			}
		default:
			if err := write(buffer, columnType, args[columnNum]); err != nil {
				return fmt.Errorf("Column %s (%s): %s", column, columnType, err.Error())
			}
		}
	}
	return nil
}