package dataset_source

import (
	"context"
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
)

const defaultBatchSize = 2048

type ParquetFileDataSource struct {
	columns []string

	fileReader   *file.Reader
	recordReader pqarrow.RecordReader

	currentRecord arrow.Record
	currentOffset int64
	circular      bool
}

func NewParquetFileDataSource(filePath string, offset int64, circular bool) (*ParquetFileDataSource, error) {
	var rdr, err = file.OpenParquetFile(filePath, true)
	if err != nil {
		return nil, fmt.Errorf("error opening parquet file: %v", err)
	}

	// Create Arrow reader for reading rows as Arrow records
	var mem = memory.NewGoAllocator()
	var reader *pqarrow.FileReader
	if reader, err = pqarrow.NewFileReader(rdr, pqarrow.ArrowReadProperties{
		BatchSize: defaultBatchSize,
	}, mem); err != nil {
		return nil, fmt.Errorf("error creating Arrow file reader: %v", err)
	}

	var fileMetadata = rdr.MetaData()

	var columns []int
	var columnNames []string
	for i := 0; i < fileMetadata.Schema.NumColumns(); i++ {
		var columnName = fileMetadata.Schema.Column(i).Path()
		columns = append(columns, i)
		columnNames = append(columnNames, columnName)
	}

	var rgrs []int
	for r := 0; r < rdr.NumRowGroups(); r++ {
		rgrs = append(rgrs, r)
	}

	var recordReader pqarrow.RecordReader
	if recordReader, err = reader.GetRecordReader(context.Background(), columns, rgrs); err != nil {
		return nil, fmt.Errorf("error creating record reader: %v", err)
	}

	var source = &ParquetFileDataSource{
		columns:      columnNames,
		fileReader:   rdr,
		recordReader: recordReader,
		circular:     circular,
	}

	if skipErr := source.skipUntilOffset(offset); skipErr != nil {
		return nil, skipErr
	}

	return source, nil
}

func min(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func (ds *ParquetFileDataSource) skipUntilOffset(rowsToSkip int64) error {
	var skipCount int64 = 0

	for ds.currentOffset < rowsToSkip {
		if ds.currentRecord == nil {
			if !ds.recordReader.Next() {
				if ds.circular {
					ds.resetReader()
					if !ds.recordReader.Next() {
						return fmt.Errorf("failed to read after reset")
					}
					rowsToSkip -= skipCount
				} else {
					return nil
				}
			}
			ds.currentRecord = ds.recordReader.Record()
		}

		remainingInRecord := ds.currentRecord.NumRows() - ds.currentOffset
		skipCount = min(rowsToSkip-ds.currentOffset, remainingInRecord)

		ds.currentOffset += skipCount

		if ds.currentOffset >= ds.currentRecord.NumRows() {
			ds.currentRecord.Release()
			ds.currentRecord = nil
			ds.currentOffset = 0
		}
	}

	return nil
}

func (ds *ParquetFileDataSource) GetColumnNames() []string {
	return ds.columns
}

func (ds *ParquetFileDataSource) GetNextRow() ([]interface{}, error) {
	if ds.currentRecord == nil {
		if ds.recordReader.Next() {
			ds.currentRecord = ds.recordReader.Record()
		} else if ds.circular {
			ds.resetReader()
			if !ds.recordReader.Next() {
				return nil, fmt.Errorf("failed to read after reset")
			}
			ds.currentRecord = ds.recordReader.Record()
		} else {
			return nil, nil
		}
	}

	var row = make([]interface{}, 0, len(ds.columns))
	for _, col := range ds.currentRecord.Columns() {
		// var colData = col.GetOneForMarshal(int(ds.currentOffset))
		var colData interface{}
		switch specificArray := col.(type) {
		case *array.Int64:
			colData = specificArray.Value(int(ds.currentOffset))
		case *array.Float64:
			colData = specificArray.Value(int(ds.currentOffset))
		case *array.String:
			colData = specificArray.Value(int(ds.currentOffset))
		case *array.Binary:
			colData = specificArray.Value(int(ds.currentOffset))
		case *array.List:
			var beg, end = specificArray.ValueOffsets(int(ds.currentOffset))
			var values = array.NewSlice(specificArray.ListValues(), beg, end)
			switch specificNestedArray := values.(type) {
			case *array.Float32:
				colData = specificNestedArray.Float32Values()
			}
			values.Release()
		}

		row = append(row, colData)
	}
	ds.currentOffset++

	if ds.currentOffset >= ds.currentRecord.NumRows() {
		ds.currentRecord.Release()
		ds.currentRecord = nil
		ds.currentOffset = 0
	}

	return row, nil
}

func (ds *ParquetFileDataSource) Close() {
	ds.recordReader.Release()
	ds.fileReader.Close()
}

func (ds *ParquetFileDataSource) resetReader() {
	if ds.currentRecord != nil {
		ds.currentRecord.Release()
		ds.currentRecord = nil
	}
	ds.recordReader.Release()

	mem := memory.NewGoAllocator()
	reader, err := pqarrow.NewFileReader(ds.fileReader, pqarrow.ArrowReadProperties{
		BatchSize: defaultBatchSize,
	}, mem)
	if err != nil {
		panic(fmt.Sprintf("error creating Arrow file reader: %v", err))
	}

	var columns []int
	for i := 0; i < ds.fileReader.MetaData().Schema.NumColumns(); i++ {
		columns = append(columns, i)
	}

	var rgrs []int
	for r := 0; r < ds.fileReader.NumRowGroups(); r++ {
		rgrs = append(rgrs, r)
	}

	recordReader, err := reader.GetRecordReader(context.Background(), columns, rgrs)
	if err != nil {
		panic(fmt.Sprintf("error creating record reader: %v", err))
	}

	ds.recordReader = recordReader
	ds.currentOffset = 0
}
